package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// wallet.go is the consistency-path half of money movement (spec §4.2):
// synchronous, Postgres-direct, no Redis fast-path shortcuts.
//
// Deposits: initiated here through the PaymentProvider interface (see
// payment.go), so this file doesn't know or care whether Daraja or
// Palpluss (PAYMENT_PROVIDER env var) is actually handling collection.
// Both are fully implemented end to end (daraja.go / palpluss.go),
// including their respective webhook callbacks below and in palpluss.go.
//
// Withdrawals: intentionally stop at creating a 'pending' record. There is
// NO B2C payout integration for either provider here — that's wired up
// separately. Do not add a payout call to this file without B2C
// credentials configured; ReviewWithdrawal in db.go already debits the
// balance on approval so the money is reserved and can't be double-spent
// while the manual payout step happens outside this codebase.

const (
	minDepositKES    = 200.0
	maxWithdrawalKES = 100000.0
)

// WalletTransaction is the shape the wallet page (wallet.tsx, via
// api/wallet.ts's Transaction interface) actually reads — PascalCase, and
// MpesaReceipt/ReviewedBy serialize as explicit null (no omitempty) since
// the frontend type treats them as `string | null`, not optional. Kept
// separate from db.go's DashboardTransaction, which uses a different
// casing/shape for the admin dashboard — reusing that directly is what
// broke this page.
type WalletTransaction struct {
	ID           uuid.UUID `json:"ID"`
	UserID       uuid.UUID `json:"UserID"`
	Type         string    `json:"Type"`
	Amount       float64   `json:"Amount"`
	Status       string    `json:"Status"`
	MpesaReceipt *string   `json:"MpesaReceipt"`
	ReviewedBy   *string   `json:"ReviewedBy"`
	CreatedAt    time.Time `json:"CreatedAt"`
	UpdatedAt    time.Time `json:"UpdatedAt"`
}

// kenyanPhoneRe matches the full Daraja-ready format: 254 followed by a
// Safaricom/other mobile prefix (07... or 01... with the leading 0 dropped)
// and 8 more digits, e.g. 254712345678 or 254101234567. The frontend now
// only collects the part after "254" (see signup.tsx / wallet.tsx), so this
// is mainly a sanity check against a tampered or malformed request body.
var kenyanPhoneRe = regexp.MustCompile(`^254[17]\d{8}$`)

func isValidKenyanPhone(phone string) bool {
	return kenyanPhoneRe.MatchString(phone)
}

// getLiveBalance is the shared read path for both demo and real balance.
// Redis (balance:{userId}) is authoritative once warm — it's what the fast
// path (game.go's bet/cashout) updates instantly, and now also what every
// consistency-path write (deposits, withdrawal approvals, influencer
// credits) syncs into via RDB.IncrRealBalance. Postgres is only consulted
// to warm the cache on first touch. See db.go's FlushRound comment and
// redis.go's IncrRealBalance comment for the fuller picture.
func (a *App) getLiveBalance(ctx context.Context, userID uuid.UUID) (demo, real float64, err error) {
	cached, err := a.rdb.HasBalance(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	if !cached {
		profile, perr := a.db.GetProfile(ctx, userID)
		if perr != nil {
			return 0, 0, perr
		}
		if err := a.rdb.EnsureBalance(ctx, userID, profile.DemoBalance, profile.RealBalance); err != nil {
			return 0, 0, err
		}
		return profile.DemoBalance, profile.RealBalance, nil
	}
	return a.rdb.GetBalance(ctx, userID)
}

type depositRequest struct {
	Amount float64 `json:"amount"`
	Phone  string  `json:"phone"` // 2547XXXXXXXX format expected by Daraja
}

func (a *App) InitiateDeposit(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())

	var req depositRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Amount < minDepositKES {
		writeError(w, http.StatusBadRequest, "minimum deposit is KES 200")
		return
	}
	if !isValidKenyanPhone(req.Phone) {
		writeError(w, http.StatusBadRequest, "enter a valid M-Pesa number (07... or 01...)")
		return
	}

	txID, err := a.db.CreateDeposit(r.Context(), userID, req.Amount, req.Phone, a.payments.Name())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create deposit record")
		return
	}

	result, err := a.payments.InitiateDeposit(r.Context(), req.Phone, req.Amount, txID.String())
	if err != nil {
		_ = a.db.FailDeposit(r.Context(), txID, err.Error())
		writeError(w, http.StatusBadGateway, fmt.Sprintf("failed to initiate %s payment: %s", a.payments.Name(), err.Error()))
		return
	}

	if err := a.db.SetDepositCheckoutID(r.Context(), txID, result.ProviderRef); err != nil {
		log.Printf("wallet: failed to attach checkout id for tx %s: %v", txID, err)
	}

	writeSuccess(w, map[string]any{
		"transactionId":     txID,
		"checkoutRequestId": result.ProviderRef,
		"message":           result.Message,
	})
}

// DarajaCallback is the unauthenticated webhook Safaricom hits once the
// customer completes (or cancels/fails) the STK prompt on their phone.
func (a *App) DarajaCallback(w http.ResponseWriter, r *http.Request) {
	var payload STKCallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		// Always 200 back to Safaricom even on a parse failure — retrying
		// a malformed callback won't fix itself, and non-200 just triggers
		// their retry storm.
		w.WriteHeader(http.StatusOK)
		return
	}

	cb := payload.Body.StkCallback
	txID, err := a.db.GetDepositByCheckoutID(r.Context(), cb.CheckoutRequestID)
	if err != nil {
		log.Printf("wallet: daraja callback for unknown checkout id %s: %v", cb.CheckoutRequestID, err)
		w.WriteHeader(http.StatusOK)
		return
	}

	receipt, _, ok := payload.ExtractReceipt()
	if !ok {
		if err := a.db.FailDeposit(r.Context(), txID, cb.ResultDesc); err != nil {
			log.Printf("wallet: failed to mark deposit %s failed: %v", txID, err)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	depositUserID, depositAmount, credited, err := a.db.CompleteDeposit(r.Context(), txID, receipt)
	if err != nil {
		log.Printf("wallet: failed to complete deposit %s: %v", txID, err)
	} else if credited {
		// Postgres is now ahead of Redis for this user — push the delta
		// through so a cached balance doesn't shadow the deposit until
		// something happens to evict it (see IncrRealBalance's doc comment).
		if err := a.rdb.IncrRealBalance(r.Context(), depositUserID, depositAmount); err != nil {
			log.Printf("wallet: failed to sync redis balance after deposit %s: %v", txID, err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *App) GetWalletBalance(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	_, real, err := a.getLiveBalance(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch balance")
		return
	}
	writeSuccess(w, map[string]any{"realBalance": real})
}

// GetWalletTransactions previously called the admin dashboard's
// ListTransactions (the 50 most recent transactions across ALL users, only
// then filtered down to this one) and wrapped the result as
// {transactions, total}. Two bugs followed from that: a user's own history
// could be pushed off the list entirely by other users' activity, and
// wallet.tsx does `res.data.filter(...)` / `res.data.map(...)` expecting an
// array — an object with a "transactions" key isn't one, so opening the
// page threw immediately.
func (a *App) GetWalletTransactions(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	txs, err := a.db.ListUserTransactions(r.Context(), userID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch transactions")
		return
	}
	out := make([]WalletTransaction, len(txs))
	for i, t := range txs {
		var reviewedBy *string
		if t.ReviewedBy != nil {
			s := t.ReviewedBy.String()
			reviewedBy = &s
		}
		out[i] = WalletTransaction{
			ID:           t.ID,
			UserID:       t.UserID,
			Type:         t.Type,
			Amount:       t.Amount,
			Status:       t.Status,
			MpesaReceipt: t.MpesaReceipt,
			ReviewedBy:   reviewedBy,
			CreatedAt:    t.CreatedAt,
			UpdatedAt:    t.UpdatedAt,
		}
	}
	writeSuccess(w, out)
}

type withdrawRequest struct {
	Amount float64 `json:"amount"`
	Phone  string  `json:"phone"` // 2547XXXXXXXX / 2541XXXXXXXX — where the payout should land
}

// InitiateWithdrawal creates a pending withdrawal record only. See the file
// header — there is no payout logic here by design, this stops at "pending"
// and waits for the (not-yet-built) B2C flow or manual admin action.
func (a *App) InitiateWithdrawal(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())

	var req withdrawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Amount <= 0 || req.Amount > maxWithdrawalKES {
		writeError(w, http.StatusBadRequest, "withdrawal amount must be between 1 and 100,000")
		return
	}
	if !isValidKenyanPhone(req.Phone) {
		writeError(w, http.StatusBadRequest, "enter a valid M-Pesa number to withdraw to (07... or 01...)")
		return
	}

	balance, err := a.db.GetProfileBalance(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check balance")
		return
	}
	if balance < req.Amount {
		writeError(w, http.StatusBadRequest, "insufficient balance")
		return
	}

	txID, err := a.db.CreateWithdrawal(r.Context(), userID, req.Amount, req.Phone)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create withdrawal request")
		return
	}

	writeSuccess(w, map[string]any{
		"transactionId": txID,
		"status":        "pending",
		"message":       "Withdrawal request received and is pending admin review.",
	})
}
