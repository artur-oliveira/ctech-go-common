package byok

import (
	"context"
	"time"
)

// Provider is one bank/PSP. Every method takes the credentials per call.
// t is the bearer for the capability in use (Token with that capability).
type Provider interface {
	Name() string
	CredentialFields() []Field

	Connect(ctx context.Context, c Credentials) (Connected, error)
	Token(ctx context.Context, c Credentials, set Capability) (Token, error)

	CreateDueCharge(ctx context.Context, c Credentials, t Token, in DueCharge) (Charge, error)
	GetCharge(ctx context.Context, c Credentials, t Token, ref string) (Charge, error)
	UpdateCharge(ctx context.Context, c Credentials, t Token, ref string, amount int64) (Charge, error)
	CancelCharge(ctx context.Context, c Credentials, t Token, ref string) error
	Refund(ctx context.Context, c Credentials, t Token, in RefundRequest) (Refund, error)
	GetRefund(ctx context.Context, c Credentials, t Token, paymentID, refundID string) (Refund, error)

	StatementPage(ctx context.Context, c Credentials, t Token, from, to Date, page int) (StatementPage, error)
	Balance(ctx context.Context, c Credentials, t Token, at Date) (Balance, error)

	GetWebhook(ctx context.Context, c Credentials, t Token, product Product) (url string, err error)
	RegisterWebhook(ctx context.Context, c Credentials, t Token, product Product, url string) error
	DeleteWebhook(ctx context.Context, c Credentials, t Token, product Product) error
	ListCallbacks(ctx context.Context, c Credentials, t Token, since time.Time) ([]CallbackAttempt, error)
	RetryCallbacks(ctx context.Context, c Credentials, t Token, refs []string) error

	// SimulatePayment pays an open charge in the provider's sandbox only.
	SimulatePayment(ctx context.Context, c Credentials, t Token, ref string) error

	// ParseCallback runs with no credentials. Its output only wakes the caller.
	ParseCallback(product Product, headers map[string]string, body []byte) (Callback, error)
}

// Payouts is the optional capability of a provider that can pay a Pix key
// (CapabilityPayout). It is a separate interface so a provider that cannot,
// or an integration not entitled to, never has to stub it.
type Payouts interface {
	// Payout asks the bank to pay. A nil error means the bank took the request,
	// not that the money arrived: read the outcome with GetPayout.
	Payout(ctx context.Context, c Credentials, t Token, in PayoutRequest) (Payout, error)
	// GetPayout reads a payout by the bank's own id. ErrNotFound means the bank
	// does not know it; the caller decides what that means.
	GetPayout(ctx context.Context, c Credentials, t Token, providerRef string) (Payout, error)
}
