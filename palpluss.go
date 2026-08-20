package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// palpluss.go is the Palpluss counterpart to daraja.go, wired in as an
// alternate PaymentProvider (see payment.go) behind PAYMENT_PROVIDER=palpluss.
//
// C2B (deposit collection) is intentionally left UNIMPLEMENTED here, same
// pattern as daraja.go's B2C gap: InitiateDeposit below is a stub that
// always errors until the actual Palpluss API call is filled in. Nothing
// else in the codebase assumes it exists — wallet.go only ever talks to it
// through the PaymentProvider interface, so wiring it up is a self-contained
// change to this one method (plus PalplussCallback below, for receiving the
// async payment confirmation once you know its payload shape).

type PalplussClient struct {
	env            string
	channelID      string
	apiKey         string
	basicAuthToken string
	callbackURL    string
	httpClient     *http.Client
}

func NewPalplussClient(cfg Config) *PalplussClient {
	return &PalplussClient{
		env:            cfg.PalplussEnv,
		channelID:      cfg.PalplussChannelID,
		apiKey:         cfg.PalplussAPIKey,
		basicAuthToken: cfg.PalplussBasicAuthToken,
		callbackURL:    cfg.PalplussCallbackURL,
		httpClient:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *PalplussClient) Name() string { return "palpluss" }

// InitiateDeposit is intentionally unimplemented — wire up the real
// Palpluss collection request here. Keep the *DepositInitResult return
// shape so wallet.go doesn't need to change: ProviderRef should be
// whatever Palpluss gives you back to match the async callback to the
// pending transaction (transactions.provider_checkout_id — see the
// migration note in db.go; that column was renamed from
// daraja_checkout_id since it's now shared between providers).
func (p *PalplussClient) InitiateDeposit(ctx context.Context, phone string, amount float64, accountReference string) (*DepositInitResult, error) {
	return nil, fmt.Errorf("palpluss: InitiateDeposit not implemented yet")
}

// PalplussCallback is the (not yet implemented) counterpart to wallet.go's
// DarajaCallback — Palpluss's async payment-confirmation webhook almost
// certainly has a different payload shape than Safaricom's, so it needs its
// own parsing once that's known. Routed at /api/wallet/palpluss/callback
// (see server.go), unauthenticated, same as the Daraja webhook.
//
// Always ack with 200 in the meantime for the same reason DarajaCallback
// does: whatever provider is on the other end will just retry a non-200,
// and a malformed/unrecognized callback won't fix itself by retrying.
func (a *App) PalplussCallback(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
