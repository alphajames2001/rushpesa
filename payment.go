package main

import "context"

// payment.go abstracts deposit collection (Daraja calls it C2B/STK Push)
// behind a single interface so wallet.go doesn't need to know which
// provider is active. Withdrawal payouts (B2C) remain out of scope for
// both providers today — see the file header comments in daraja.go and
// palpluss.go.
//
// Which provider is active is controlled entirely by the PAYMENT_PROVIDER
// env var (see server.go's loadConfig). Switching back to Daraja later is
// just flipping that one value back — daraja.go is untouched and stays
// fully wired up.

type PaymentProvider interface {
	// InitiateDeposit kicks off a payment prompt to phone for amount,
	// tagged with accountReference (the internal transaction id) so the
	// provider's async callback can be matched back to the pending
	// deposit record (see wallet.go's InitiateDeposit/DarajaCallback and
	// palpluss.go's PalplussCallback).
	InitiateDeposit(ctx context.Context, phone string, amount float64, accountReference string) (*DepositInitResult, error)

	// Name identifies the active provider ("daraja" | "palpluss") — used
	// for logging and to tag which provider a transaction went through
	// (transactions.payment_provider, see db.go's CreateDeposit).
	Name() string
}

// DepositInitResult is the provider-agnostic shape wallet.go needs back
// from InitiateDeposit, regardless of which provider produced it.
type DepositInitResult struct {
	ProviderRef string // Daraja: CheckoutRequestID. Palpluss: their STK response's transactionId (also what shows up as transaction.id on the webhook).
	Message     string // Customer-facing status message, if the provider gives one.
}

// NewPaymentProvider picks the active provider based on cfg.PaymentProvider.
// Defaults to "daraja" for anything unset/unrecognized so existing
// deployments don't need to change anything to keep working.
func NewPaymentProvider(cfg Config) PaymentProvider {
	switch cfg.PaymentProvider {
	case "palpluss":
		return NewPalplussClient(cfg)
	default:
		return NewDarajaClient(cfg)
	}
}
