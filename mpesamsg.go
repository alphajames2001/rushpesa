package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// mpesamsg.go: the mock M-PESA inbox. Messages are composed server-side in
// the real SMS format, stored (so reloads show identical text), shown by
// mpesa-messages.html and pushed to the user's registered devices over FCM.

const defaultMpesaSender = "PALPLUS FUTURE"

// Kenya has no DST, so a fixed zone avoids depending on tzdata being present.
var nairobiZone = time.FixedZone("EAT", 3*60*60)

const mpesaCodeChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// mpesaCode mimics real M-PESA codes: year letter (U = 2026), month letter
// (A..L), day (1-9 then A..), then 7 random characters.
func mpesaCode(t time.Time) string {
	t = t.In(nairobiZone)
	y := t.Year() - 2026
	if y < 0 {
		y = 0
	}
	if y > 5 {
		y = 5
	}
	day := byte('0' + t.Day())
	if t.Day() >= 10 {
		day = byte(55 + t.Day())
	}
	b := []byte{byte(65 + 20 + y), byte(64 + int(t.Month())), day}
	for i := 0; i < 7; i++ {
		b = append(b, mpesaCodeChars[rand.IntN(len(mpesaCodeChars))])
	}
	return string(b)
}

// formatKsh renders 1234.5 as "1,234.50".
func formatKsh(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	intPart, frac := s[:len(s)-3], s[len(s)-3:]
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String() + frac
}

func composeMpesaBody(t time.Time, amount, balance float64, sender string) string {
	n := t.In(nairobiZone)
	return fmt.Sprintf(
		"%s Confirmed.You have received Ksh%s from %s on %d/%d/%02d at %s. New M-PESA balance is Ksh%s. Transaction cost, Ksh0.00.",
		mpesaCode(n), formatKsh(amount), sender, n.Day(), int(n.Month()), n.Year()%100,
		n.Format("3:04 PM"), formatKsh(balance))
}

// storeMpesaMessage composes and saves a message. Returns (nil, nil) when
// withdrawalID already has one.
func (a *App) storeMpesaMessage(ctx context.Context, userID uuid.UUID, amount, balance float64, sender string, withdrawalID *uuid.UUID) (*MpesaMessage, error) {
	body := composeMpesaBody(time.Now(), amount, balance, sender)
	return a.db.InsertMpesaMessage(ctx, userID, amount, sender, body, withdrawalID)
}

// pushMpesaMessage sends msg to every registered device of its owner and
// prunes dead tokens. Returns counts for diagnostics.
func (a *App) pushMpesaMessage(ctx context.Context, msg *MpesaMessage) (devices, sent, failed int) {
	if a.push == nil {
		return 0, 0, 0
	}
	tokens, err := a.db.ListDeviceTokens(ctx, msg.UserID)
	if err != nil {
		log.Printf("push: listing device tokens for %s: %v", msg.UserID, err)
		return 0, 0, 0
	}
	for _, tok := range tokens {
		err := a.push.Send(ctx, tok, msg.Sender, msg.Body, msg.ID.String())
		switch {
		case err == nil:
			sent++
		case errors.Is(err, errInvalidToken):
			failed++
			if derr := a.db.DeleteDeviceToken(ctx, tok); derr != nil {
				log.Printf("push: deleting dead token: %v", derr)
			}
		default:
			failed++
			log.Printf("push: send to %s failed: %v", msg.UserID, err)
		}
	}
	return len(tokens), sent, failed
}

// notifyWithdrawalSent is the real trigger: called when an admin marks an
// influencer withdrawal as sent. Errors are logged, never returned, because
// the status change itself already succeeded.
func (a *App) notifyWithdrawalSent(ctx context.Context, withdrawalID uuid.UUID) {
	userID, amount, err := a.db.GetInfluencerWithdrawal(ctx, withdrawalID)
	if err != nil {
		log.Printf("mpesa msg: loading withdrawal %s: %v", withdrawalID, err)
		return
	}
	// InfluencerWithdraw already added this amount to the mock M-PESA balance
	// when the withdrawal was requested, so the balance is shown as it is now.
	balance, err := a.db.GetOrCreateMockMpesa(ctx, userID)
	if err != nil {
		log.Printf("mpesa msg: loading mock balance for %s: %v", userID, err)
		return
	}
	msg, err := a.storeMpesaMessage(ctx, userID, amount, balance, defaultMpesaSender, &withdrawalID)
	if err != nil {
		log.Printf("mpesa msg: storing message for withdrawal %s: %v", withdrawalID, err)
		return
	}
	if msg == nil {
		return // already notified for this withdrawal
	}
	go func() {
		pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a.pushMpesaMessage(pctx, msg)
	}()
}

// ---- HTTP handlers (mounted under /api/debug, see server.go) ----

type deviceTokenRequest struct {
	Token string `json:"token"`
}

// RegisterDeviceToken: POST /api/debug/device-token {token}
func (a *App) RegisterDeviceToken(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	var req deviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" || len(req.Token) > 4096 {
		writeError(w, http.StatusBadRequest, "a valid device token is required")
		return
	}
	if err := a.db.UpsertDeviceToken(r.Context(), userID, req.Token); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register device")
		return
	}
	writeSuccess(w, map[string]any{"registered": true})
}

func mpesaMessageJSON(m MpesaMessage) map[string]any {
	return map[string]any{"id": m.ID, "text": m.Body, "createdAt": m.CreatedAt}
}

// ListMyMpesaMessages: GET /api/debug/mpesa/messages
func (a *App) ListMyMpesaMessages(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	msgs, err := a.db.ListMpesaMessages(r.Context(), userID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, mpesaMessageJSON(m))
	}
	writeSuccess(w, map[string]any{"messages": out})
}

type simulateMpesaRequest struct {
	Amount float64 `json:"amount"`
	Sender string  `json:"sender"`
}

// SimulateMpesaPayment: POST /api/debug/mpesa/simulate {amount, sender?}
// Creates a message for the caller and pushes it right away. It does not
// change any balance; the shown "new balance" is the current mock M-PESA
// balance plus the simulated amount.
func (a *App) SimulateMpesaPayment(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	var req simulateMpesaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount <= 0 || req.Amount > 1_000_000 {
		writeError(w, http.StatusBadRequest, "amount must be between 0 and 1,000,000")
		return
	}
	sender := strings.ToUpper(strings.TrimSpace(req.Sender))
	if sender == "" {
		sender = defaultMpesaSender
	}
	if len(sender) > 40 {
		sender = sender[:40]
	}

	balance, err := a.db.GetOrCreateMockMpesa(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load mock balance")
		return
	}
	msg, err := a.storeMpesaMessage(r.Context(), userID, req.Amount, balance+req.Amount, sender, nil)
	if err != nil || msg == nil {
		writeError(w, http.StatusInternalServerError, "failed to create message")
		return
	}

	pctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	devices, sent, failed := a.pushMpesaMessage(pctx, msg)

	writeSuccess(w, map[string]any{
		"message": mpesaMessageJSON(*msg),
		"push": map[string]any{
			"configured": a.push != nil,
			"devices":    devices,
			"sent":       sent,
			"failed":     failed,
		},
	})
}
