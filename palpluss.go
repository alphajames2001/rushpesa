package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// palpluss.go is the Palpluss counterpart to daraja.go, wired in as an
// alternate PaymentProvider (see payment.go) behind PAYMENT_PROVIDER=palpluss.
// Implemented against https://docs.palpluss.com (Quickstart, Authentication,
// Webhooks, and the STK Push API reference).
//
// Auth: HTTP Basic, API key as username, empty password (see setAuth below).
// C2B here means PalPluss's STK Push (POST /payments/stk) — the collection
// side. B2C payouts are a separate PalPluss endpoint and, same as Daraja's
// B2C, are not wired up anywhere in this codebase.

type PalplussClient struct {
	env            string // "sandbox" | "production"
	apiKey         string
	basicAuthToken string
	callbackURL    string
	httpClient     *http.Client

	// defaultChannelID is the legacy single-channel fallback
	// (PALPLUSS_CHANEL_ID), used when no rotation pool is configured.
	defaultChannelID string
	// channels is the configured rotation pool (PALPLUSS_CHANNEL_IDS) —
	// see server.go's PalplussChannel / parsePalplussChannels.
	channels []PalplussChannel
	// rdb resolves whichever channel an admin has picked as active (see
	// admin.go's SetActivePalplussChannel). May be nil in tests; resolution
	// falls back to the pool/default in that case.
	rdb *RDB
}

func NewPalplussClient(cfg Config, rdb *RDB) *PalplussClient {
	return &PalplussClient{
		env:              cfg.PalplussEnv,
		apiKey:           cfg.PalplussAPIKey,
		basicAuthToken:   cfg.PalplussBasicAuthToken,
		callbackURL:      cfg.PalplussCallbackURL,
		httpClient:       &http.Client{Timeout: 15 * time.Second},
		defaultChannelID: cfg.PalplussChannelID,
		channels:         cfg.PalplussChannels,
		rdb:              rdb,
	}
}

// resolveChannelID picks which Palpluss channel (i.e. which wallet) a
// deposit routes to. Priority order:
//  1. The admin-selected active channel in Redis (see admin.go's
//     SetActivePalplussChannel) — this is what lets ops rotate wallets from
//     the dashboard with zero redeploy.
//  2. The first entry in the configured PALPLUSS_CHANNEL_IDS pool, if no
//     admin selection has been made yet.
//  3. The legacy single PALPLUSS_CHANEL_ID env var, for deployments that
//     haven't set up a rotation pool at all.
func (p *PalplussClient) resolveChannelID(ctx context.Context) string {
	if p.rdb != nil {
		if active, err := p.rdb.GetActivePalplussChannel(ctx); err == nil && active != "" {
			return active
		}
	}
	if len(p.channels) > 0 {
		return p.channels[0].ID
	}
	return p.defaultChannelID
}

func (p *PalplussClient) Name() string { return "palpluss" }

func (p *PalplussClient) baseURL() string {
	if p.env == "production" {
		return "https://api.palpluss.com/v1"
	}
	return "https://sandbox.palpluss.com/v1"
}

// setAuth applies HTTP Basic Auth per docs.palpluss.com/guides/authentication:
// "Authorization: Basic <base64(apikey:)>" — API key as username, empty
// password. If PALPLUSS_BASIC_AUTH_TOKEN is set, it's used verbatim as the
// already-encoded token instead (some deployments precompute this rather
// than store the raw key); otherwise it's derived from the API key via
// SetBasicAuth, which base64-encodes "apiKey:" itself.
func (p *PalplussClient) setAuth(req *http.Request) {
	if p.basicAuthToken != "" {
		req.Header.Set("Authorization", "Basic "+p.basicAuthToken)
		return
	}
	req.SetBasicAuth(p.apiKey, "")
}

// ---- POST /payments/stk ----

type palplussStkRequest struct {
	Amount           float64 `json:"amount"`
	Phone            string  `json:"phone"`
	AccountReference string  `json:"accountReference"` // max 12 chars — shown on customer's M-Pesa statement
	TransactionDesc  string  `json:"transactionDesc"`  // max 13 chars — shown on customer's PIN prompt
	ChannelID        string  `json:"channelId,omitempty"`
	CallbackURL      string  `json:"callbackUrl"`
}

type palplussStkResponse struct {
	TransactionID      string  `json:"transactionId"`
	Status             string  `json:"status"`
	Amount             float64 `json:"amount"`
	Currency           string  `json:"currency"`
	Phone              string  `json:"phone"`
	ProviderCheckoutID *string `json:"providerCheckoutId"`
	ResultCode         *string `json:"resultCode"`
	ResultDescription  *string `json:"resultDescription"`
}

type palplussSuccessEnvelope struct {
	Success   bool                `json:"success"`
	Data      palplussStkResponse `json:"data"`
	RequestID string              `json:"requestId"`
}

type palplussErrorEnvelope struct {
	Success bool `json:"success"`
	Error   struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
	RequestID string `json:"requestId"`
}

// InitiateDeposit sends the M-Pesa STK prompt via PalPluss. accountReference
// is our internal transaction id, but PalPluss caps accountReference at 12
// chars (it lands on the customer's statement), so the UUID is shortened —
// that's fine because matching back to our pending deposit doesn't rely on
// it. InitiateDeposit's ProviderRef return (PalPluss's own transactionId) is
// what actually gets stored (see wallet.go -> db.go's SetDepositCheckoutID)
// and matched against transaction.id on the webhook (PalplussCallback below).
func (p *PalplussClient) InitiateDeposit(ctx context.Context, phone string, amount float64, accountReference string) (*DepositInitResult, error) {
	if p.callbackURL == "" {
		return nil, fmt.Errorf("palpluss: PALPLUSS_CALLBACK_URL is not configured")
	}

	ref := strings.ReplaceAll(accountReference, "-", "")
	if len(ref) > 12 {
		ref = ref[:12]
	}

	body := palplussStkRequest{
		Amount:           amount,
		Phone:            phone,
		AccountReference: ref,
		TransactionDesc:  "Deposit", // 7 chars, comfortably under the 13-char cap
		ChannelID:        p.resolveChannelID(ctx),
		CallbackURL:      p.callbackURL,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL()+"/payments/stk", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	p.setAuth(req)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("palpluss stk push request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		var errEnv palplussErrorEnvelope
		if jerr := json.Unmarshal(raw, &errEnv); jerr == nil && errEnv.Error.Message != "" {
			return nil, fmt.Errorf("palpluss: %s (%s)", errEnv.Error.Message, errEnv.Error.Code)
		}
		return nil, fmt.Errorf("palpluss stk push failed (%d): %s", resp.StatusCode, string(raw))
	}

	var out palplussSuccessEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unexpected palpluss stk response: %s", string(raw))
	}

	return &DepositInitResult{
		ProviderRef: out.Data.TransactionID,
		Message:     "STK prompt sent — enter your M-Pesa PIN to complete the deposit.",
	}, nil
}

// ---- Webhook callback ----

// PalplussCallbackPayload matches docs.palpluss.com/guides/webhooks exactly.
// event_type is one of transaction.success | transaction.failed |
// transaction.cancelled | transaction.expired; only .success credits the
// deposit, everything else is treated as a failure the same way Daraja's
// non-zero ResultCode is.
type PalplussCallbackPayload struct {
	Event       string `json:"event"`
	EventType   string `json:"event_type"`
	Transaction struct {
		ID                 string  `json:"id"`
		TenantID           string  `json:"tenant_id"`
		Type               string  `json:"type"`
		Status             string  `json:"status"`
		Amount             float64 `json:"amount"`
		Currency           string  `json:"currency"`
		PhoneNumber        string  `json:"phone_number"`
		ExternalReference  string  `json:"external_reference"`
		Provider           string  `json:"provider"`
		ProviderRequestID  string  `json:"provider_request_id"`
		ProviderCheckoutID string  `json:"provider_checkout_id"`
		MpesaReceipt       *string `json:"mpesa_receipt"`
		ResultCode         string  `json:"result_code"`
		ResultDesc         string  `json:"result_desc"`
	} `json:"transaction"`
}

// PalplussCallback is PalPluss's counterpart to wallet.go's DarajaCallback.
// PalPluss doesn't sign these (nothing in their docs about a signature
// header), and it can retry the same delivery up to 5 times on anything but
// a 2xx, so this always acks 200 and relies on db.CompleteDeposit's existing
// status check for idempotency — the same guard DarajaCallback leans on for
// duplicate M-Pesa retries.
func (a *App) PalplussCallback(w http.ResponseWriter, r *http.Request) {
	var payload PalplussCallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		// Ack anyway — a malformed callback won't fix itself on retry, and a
		// non-2xx here just feeds PalPluss's retry schedule for nothing.
		w.WriteHeader(http.StatusOK)
		return
	}

	txn := payload.Transaction
	txID, err := a.db.GetDepositByCheckoutID(r.Context(), txn.ID)
	if err != nil {
		log.Printf("wallet: palpluss callback for unknown transaction id %s: %v", txn.ID, err)
		w.WriteHeader(http.StatusOK)
		return
	}

	if payload.EventType != "transaction.success" {
		reason := txn.ResultDesc
		if reason == "" {
			reason = payload.EventType
		}
		if err := a.db.FailDeposit(r.Context(), txID, reason); err != nil {
			log.Printf("wallet: failed to mark deposit %s failed: %v", txID, err)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	receipt := ""
	if txn.MpesaReceipt != nil {
		receipt = *txn.MpesaReceipt
	}

	depositUserID, depositAmount, credited, err := a.db.CompleteDeposit(r.Context(), txID, receipt)
	if err != nil {
		log.Printf("wallet: failed to complete deposit %s: %v", txID, err)
	} else if credited {
		// Same reasoning as DarajaCallback: Postgres is now ahead of Redis
		// for this user, push the delta through so a cached balance doesn't
		// shadow the deposit (see redis.go's IncrRealBalance doc comment).
		if err := a.rdb.IncrRealBalance(r.Context(), depositUserID, depositAmount); err != nil {
			log.Printf("wallet: failed to sync redis balance after deposit %s: %v", txID, err)
		}
	}
	w.WriteHeader(http.StatusOK)
}
