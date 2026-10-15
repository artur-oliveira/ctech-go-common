package byok

import (
	"fmt"
	"strings"
	"time"
)

// Date is a civil date (no time zone). Providers speak YYYY-MM-DD.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("byok: date %q: %w", s, err)
	}
	return Date{t.Year(), t.Month(), t.Day()}, nil
}

func DateOf(t time.Time) Date { return Date{t.Year(), t.Month(), t.Day()} }

func (d Date) time() time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }
func (d Date) String() string  { return d.time().Format(time.DateOnly) }
func (d Date) AddDays(n int) Date {
	return DateOf(d.time().AddDate(0, 0, n))
}
func (d Date) IsZero() bool       { return d == Date{} }
func (d Date) Before(o Date) bool { return d.time().Before(o.time()) }

// Capability is what a connection may be used for. Providers map their scopes
// onto these; billing never reads a provider scope name.
type Capability string

const (
	CapabilityStatement  Capability = "statement"
	CapabilityCollection Capability = "collection"

	// The products a provider may offer beyond statement and due-date Pix
	// collection. A connection reports the ones its credentials allow; nothing
	// requires them, and a token for one carries that product's scopes only.
	CapabilityImmediateCharge Capability = "immediate_charge" // immediate Pix charges and QR payload locations
	CapabilityDueBatch        Capability = "due_batch"        // batches of due-date Pix charges
	CapabilityBoleto          Capability = "boleto"           // boleto with Pix (Cobrança v3)
	CapabilityRecurring       Capability = "recurring"        // Pix Automático
	CapabilityPayout          Capability = "payout"           // Pix payouts and their payment notices
)

// Product is the webhook path segment and the callback parser selector.
type Product string

const ProductPix Product = "pix"

// Field is one credential the console asks for.
type Field struct {
	Name     string
	Secret   bool // never echoed back
	File     bool // uploaded as a file (PEM)
	Required bool
}

// ConnectionRef is a stored connection as the outbound Lambda receives it:
// the sealed bundle and what binds it (the AAD). ID "sandbox" means CTech's own
// integration against the provider's sandbox host.
type ConnectionRef struct {
	AccountOrganizationID string `json:"account_organization_id,omitempty"`
	ID                    string `json:"id"`
	Version               int    `json:"version,omitempty"`
	Sealed                string `json:"sealed,omitempty"`
	Account               string `json:"account,omitempty"`
	PixKey                string `json:"pix_key,omitempty"`
}

// Credentials is either plaintext Values (only inside the Lambda, or in the
// single Connect request) or a sealed Ref the Lambda opens.
type Credentials struct {
	Values  map[string]string
	Ref     *ConnectionRef
	Account string
	PixKey  string
	Sandbox bool
}

// Token is a provider bearer. CertNotAfter is the integration certificate's
// expiry, reported so the operator can be warned about a shared integration.
type Token struct {
	Value        string    `json:"value"`
	ExpiresIn    int       `json:"expires_in"`
	Scopes       []string  `json:"scopes,omitempty"`
	CertNotAfter time.Time `json:"cert_not_after,omitempty"`
}

// Connected is the outcome of a connection test. Sealed is set only by the
// outbound Lambda (it is the ciphertext the caller stores).
type Connected struct {
	Capabilities    []Capability `json:"capabilities"`
	ScopesGranted   []string     `json:"scopes_granted"`
	MissingScopes   []string     `json:"missing_scopes,omitempty"`
	CertSubject     string       `json:"cert_subject,omitempty"`
	CertFingerprint string       `json:"cert_fingerprint,omitempty"`
	CertNotAfter    time.Time    `json:"cert_not_after,omitempty"`
	Balance         Balance      `json:"balance"`
	Sealed          string       `json:"sealed,omitempty"`
}

type Debtor struct {
	Name  string `json:"name"`
	TaxID string `json:"tax_id"` // digits only: 11 = CPF, 14 = CNPJ
}

// DueCharge is a charge with a due date (Inter: cobv). Basis points: 200 = 2%.
type DueCharge struct {
	Ref                        string `json:"ref"`
	Due                        Date   `json:"due"`
	ValidityDays               int    `json:"validity_days"`
	Amount                     int64  `json:"amount"`
	FineBasisPoints            int    `json:"fine_bp,omitempty"`
	MonthlyInterestBasisPoints int    `json:"monthly_interest_bp,omitempty"`
	Debtor                     Debtor `json:"debtor"`
	Message                    string `json:"message,omitempty"`
}

type ChargeStatus string

const (
	ChargeOpen    ChargeStatus = "open"
	ChargePaid    ChargeStatus = "paid"
	ChargeExpired ChargeStatus = "expired"
	ChargeRemoved ChargeStatus = "removed"
)

type Payment struct {
	ID          string    `json:"id"` // Inter: endToEndId
	Amount      int64     `json:"amount"`
	LateCharges int64     `json:"late_charges,omitempty"`
	PaidAt      time.Time `json:"paid_at"`
}

type Charge struct {
	Ref      string       `json:"ref"`
	Status   ChargeStatus `json:"status"`
	Amount   int64        `json:"amount"`
	Payable  string       `json:"payable,omitempty"` // Inter: pixCopiaECola
	Payments []Payment    `json:"payments,omitempty"`
}

type RefundRequest struct {
	PaymentID string `json:"payment_id"`
	RefundID  string `json:"refund_id"`
	Amount    int64  `json:"amount"`
	Reason    string `json:"reason,omitempty"`
}

type RefundStatus string

const (
	RefundProcessing RefundStatus = "processing"
	RefundDone       RefundStatus = "done"
	RefundFailed     RefundStatus = "failed"
)

type Refund struct {
	ID        string       `json:"id"`
	PaymentID string       `json:"payment_id"`
	Amount    int64        `json:"amount"`
	Status    RefundStatus `json:"status"`
}

type Direction string

const (
	Credit Direction = "credit"
	Debit  Direction = "debit"
)

// StatementLine is one bank statement entry. Amount is always positive;
// Direction says which way it moved.
type StatementLine struct {
	ID          string    `json:"id"`
	Date        Date      `json:"date"`
	Direction   Direction `json:"direction"`
	Amount      int64     `json:"amount"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	ChargeRef   string    `json:"charge_ref,omitempty"`
	PaymentID   string    `json:"payment_id,omitempty"`
	// Payer is who sent a received payment, when the bank's line says so. Nil on
	// debits (the sender there is the account's own holder) and on lines that
	// carry no payer detail.
	Payer *Payer `json:"payer,omitempty"`
}

// Payer is the sender of a received payment, as the bank's statement names it.
// The document is whole when the bank gives it whole; callers compare it digit
// for digit and never assume it is masked or unmasked.
type Payer struct {
	Name        string `json:"name,omitempty"`
	Document    string `json:"document,omitempty"`    // CPF or CNPJ
	Institution string `json:"institution,omitempty"` // the payer's bank
}

// PayoutRequest is one payment out of the connected account to a Pix key.
// Ref is an RFC 4122 UUID chosen by the caller and stored BEFORE the call: the
// bank deduplicates on it, so repeating the request cannot pay twice.
type PayoutRequest struct {
	Ref     string `json:"ref"`
	Amount  int64  `json:"amount"` // centavos
	PixKey  string `json:"pix_key"`
	Message string `json:"message,omitempty"`
}

// PayoutStatus is where a payout stands. Anything the bank says that a provider
// does not recognise is PayoutUnknown, which a caller treats as "not settled
// yet" and raises: it is never read as paid or as failed.
type PayoutStatus string

const (
	PayoutProcessing PayoutStatus = "processing"
	PayoutDone       PayoutStatus = "done"
	PayoutFailed     PayoutStatus = "failed"
	PayoutUnknown    PayoutStatus = "unknown"
)

// Payout is the bank's view of a payment out. ProviderRef is the bank's own id
// for it (what a lookup uses); Ref is the caller's.
type Payout struct {
	Ref         string       `json:"ref,omitempty"`
	ProviderRef string       `json:"provider_ref,omitempty"`
	PaymentID   string       `json:"payment_id,omitempty"` // end-to-end id, once there is one
	Status      PayoutStatus `json:"status"`
	Reason      string       `json:"reason,omitempty"` // the bank's text when it refused
}

type StatementPage struct {
	Lines      []StatementLine `json:"lines"`
	Page       int             `json:"page"`
	TotalPages int             `json:"total_pages"`
}

type Balance struct {
	Available int64 `json:"available"`
	Blocked   int64 `json:"blocked"`
	At        Date  `json:"at"`
}

type CallbackAttempt struct {
	ChargeRef  string    `json:"charge_ref"`
	HTTPStatus int       `json:"http_status"`
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
}

// Callback is what a webhook body claims. Nothing in it is trusted: it only
// names which charges to re-query.
type Callback struct {
	Account    string   `json:"account,omitempty"`
	ChargeRefs []string `json:"charge_refs"`
	PaymentIDs []string `json:"payment_ids,omitempty"`
	// Payers is who the body says paid each charge, when it says. A claim, not a
	// fact: a consumer may use it as provisional evidence of the payer (the
	// document is usually masked) and must still settle from the charge re-query.
	Payers []CallbackPayer `json:"payers,omitempty"`
}

// CallbackPayer is the payer a webhook names for one received payment.
type CallbackPayer struct {
	ChargeRef string `json:"charge_ref"`
	PaymentID string `json:"payment_id,omitempty"`
	Name      string `json:"name,omitempty"`
	// Document is as sent: a CPF masked to some of its digits ("***137303**"), or
	// complete if the bank ever sends it so.
	Document string `json:"document,omitempty"`
}

func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte(`""`), nil
	}
	return []byte(`"` + d.String() + `"`), nil
}

func (d *Date) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" {
		*d = Date{}
		return nil
	}
	p, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = p
	return nil
}
