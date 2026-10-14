// Package byoktest is an in-memory byok.Provider for tests in every consumer.
// It is deterministic, safe for concurrent use, and never touches a network.
package byoktest

import (
	"context"
	"sort"
	"sync"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

type Fake struct {
	mu           sync.Mutex
	Capabilities []byok.Capability
	PageSize     int
	charges      map[string]*byok.Charge
	refunds      map[string]byok.Refund
	payouts      map[string]*byok.Payout // by the caller's Ref
	lines        []byok.StatementLine
	balance      byok.Balance
	balanceAt    map[byok.Date]int64
	hooks        map[byok.Product]string
	callbacks    []byok.CallbackAttempt
	failNext     map[byok.Op]error
	calls        map[byok.Op]int
	tokens       int
}

func New() *Fake {
	return &Fake{
		Capabilities: []byok.Capability{byok.CapabilityStatement, byok.CapabilityCollection},
		PageSize:     50,
		charges:      map[string]*byok.Charge{},
		refunds:      map[string]byok.Refund{},
		payouts:      map[string]*byok.Payout{},
		balanceAt:    map[byok.Date]int64{},
		hooks:        map[byok.Product]string{},
		failNext:     map[byok.Op]error{},
		calls:        map[byok.Op]int{},
	}
}

func (f *Fake) enter(op byok.Op) error {
	f.calls[op]++
	if err, ok := f.failNext[op]; ok {
		delete(f.failNext, op)
		return err
	}
	return nil
}

func (f *Fake) FailNext(op byok.Op, err error) { f.mu.Lock(); f.failNext[op] = err; f.mu.Unlock() }
func (f *Fake) Calls(op byok.Op) int           { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[op] }
func (f *Fake) SetBalance(b byok.Balance)      { f.mu.Lock(); f.balance = b; f.mu.Unlock() }
func (f *Fake) AddLines(ls ...byok.StatementLine) {
	f.mu.Lock()
	f.lines = append(f.lines, ls...)
	f.mu.Unlock()
}
func (f *Fake) SetWebhookURL(p byok.Product, url string) {
	f.mu.Lock()
	f.hooks[p] = url
	f.mu.Unlock()
}
func (f *Fake) WebhookURL(p byok.Product) string { f.mu.Lock(); defer f.mu.Unlock(); return f.hooks[p] }
func (f *Fake) AddCallback(a byok.CallbackAttempt) {
	f.mu.Lock()
	f.callbacks = append(f.callbacks, a)
	f.mu.Unlock()
}

// Pay records a payment on ref (a second Pay is a duplicate payment).
func (f *Fake) Pay(ref string, amount, late int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.charges[ref]
	if ch == nil {
		return
	}
	ch.Payments = append(ch.Payments, byok.Payment{ID: "E2E" + ref + string(rune('A'+len(ch.Payments))), Amount: amount, LateCharges: late, PaidAt: at})
	ch.Status = byok.ChargePaid
}

func (f *Fake) Charge(ref string) (byok.Charge, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.charges[ref]
	if !ok {
		return byok.Charge{}, false
	}
	return *ch, true
}

func (f *Fake) Name() string { return "fake" }
func (f *Fake) CredentialFields() []byok.Field {
	return []byok.Field{{Name: "client_id", Required: true}, {Name: "client_secret", Secret: true, Required: true}}
}

func (f *Fake) Connect(_ context.Context, c byok.Credentials) (byok.Connected, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpConnect); err != nil {
		return byok.Connected{}, err
	}
	return byok.Connected{Capabilities: f.Capabilities, ScopesGranted: []string{"fake.all"},
		CertSubject: "CN=fake", CertFingerprint: "00", CertNotAfter: time.Now().AddDate(1, 0, 0), Balance: f.balance}, nil
}

func (f *Fake) Token(_ context.Context, _ byok.Credentials, set byok.Capability) (byok.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpToken); err != nil {
		return byok.Token{}, err
	}
	f.tokens++
	return byok.Token{Value: "tok-" + string(set), ExpiresIn: 3600}, nil
}

func (f *Fake) CreateDueCharge(_ context.Context, _ byok.Credentials, _ byok.Token, in byok.DueCharge) (byok.Charge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpCreateDueCharge); err != nil {
		return byok.Charge{}, err
	}
	if ch, ok := f.charges[in.Ref]; ok {
		return *ch, nil
	}
	ch := &byok.Charge{Ref: in.Ref, Status: byok.ChargeOpen, Amount: in.Amount, Payable: "00020126FAKE" + in.Ref}
	f.charges[in.Ref] = ch
	return *ch, nil
}

func (f *Fake) GetCharge(_ context.Context, _ byok.Credentials, _ byok.Token, ref string) (byok.Charge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpGetCharge); err != nil {
		return byok.Charge{}, err
	}
	ch, ok := f.charges[ref]
	if !ok {
		return byok.Charge{}, byok.ErrNotFound
	}
	return *ch, nil
}

func (f *Fake) UpdateCharge(_ context.Context, _ byok.Credentials, _ byok.Token, ref string, amount int64) (byok.Charge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpUpdateCharge); err != nil {
		return byok.Charge{}, err
	}
	ch, ok := f.charges[ref]
	if !ok {
		return byok.Charge{}, byok.ErrNotFound
	}
	if ch.Status != byok.ChargeOpen {
		return byok.Charge{}, byok.ErrConcluded
	}
	ch.Amount = amount
	return *ch, nil
}

func (f *Fake) CancelCharge(_ context.Context, _ byok.Credentials, _ byok.Token, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpCancelCharge); err != nil {
		return err
	}
	ch, ok := f.charges[ref]
	if !ok {
		return byok.ErrNotFound
	}
	if ch.Status != byok.ChargeOpen {
		return byok.ErrConcluded
	}
	ch.Status = byok.ChargeRemoved
	return nil
}

func (f *Fake) Refund(_ context.Context, _ byok.Credentials, _ byok.Token, in byok.RefundRequest) (byok.Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpRefund); err != nil {
		return byok.Refund{}, err
	}
	if r, ok := f.refunds[in.RefundID]; ok {
		return r, nil
	}
	r := byok.Refund{ID: in.RefundID, PaymentID: in.PaymentID, Amount: in.Amount, Status: byok.RefundProcessing}
	f.refunds[in.RefundID] = r
	return r, nil
}

func (f *Fake) GetRefund(_ context.Context, _ byok.Credentials, _ byok.Token, _ string, refundID string) (byok.Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpGetRefund); err != nil {
		return byok.Refund{}, err
	}
	r, ok := f.refunds[refundID]
	if !ok {
		return byok.Refund{}, byok.ErrNotFound
	}
	r.Status = byok.RefundDone
	f.refunds[refundID] = r
	return r, nil
}

func (f *Fake) StatementPage(_ context.Context, _ byok.Credentials, _ byok.Token, from, to byok.Date, page int) (byok.StatementPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpStatementPage); err != nil {
		return byok.StatementPage{}, err
	}
	var in []byok.StatementLine
	for _, l := range f.lines {
		if !l.Date.Before(from) && !to.Before(l.Date) {
			in = append(in, l)
		}
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].ID < in[j].ID })
	total := (len(in) + f.PageSize - 1) / f.PageSize
	lo, hi := page*f.PageSize, min((page+1)*f.PageSize, len(in))
	if lo > len(in) {
		lo = len(in)
	}
	return byok.StatementPage{Lines: append([]byok.StatementLine(nil), in[lo:hi]...), Page: page, TotalPages: total}, nil
}

func (f *Fake) Balance(_ context.Context, _ byok.Credentials, _ byok.Token, at byok.Date) (byok.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpBalance); err != nil {
		return byok.Balance{}, err
	}
	b := f.balance
	if v, ok := f.balanceAt[at]; ok {
		b.Available = v
	}
	b.At = at
	return b, nil
}

// SetBalanceAt answers Balance(at) with available (an opening-balance date).
func (f *Fake) SetBalanceAt(at byok.Date, available int64) {
	f.mu.Lock()
	f.balanceAt[at] = available
	f.mu.Unlock()
}

func (f *Fake) GetWebhook(_ context.Context, _ byok.Credentials, _ byok.Token, p byok.Product) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpGetWebhook); err != nil {
		return "", err
	}
	return f.hooks[p], nil
}

func (f *Fake) RegisterWebhook(_ context.Context, _ byok.Credentials, _ byok.Token, p byok.Product, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpRegisterWebhook); err != nil {
		return err
	}
	f.hooks[p] = url
	return nil
}

func (f *Fake) DeleteWebhook(_ context.Context, _ byok.Credentials, _ byok.Token, p byok.Product) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpDeleteWebhook); err != nil {
		return err
	}
	delete(f.hooks, p)
	return nil
}

func (f *Fake) ListCallbacks(_ context.Context, _ byok.Credentials, _ byok.Token, since time.Time) ([]byok.CallbackAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpListCallbacks); err != nil {
		return nil, err
	}
	var out []byok.CallbackAttempt
	for _, a := range f.callbacks {
		if !a.At.Before(since) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *Fake) RetryCallbacks(_ context.Context, _ byok.Credentials, _ byok.Token, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enter(byok.OpRetryCallbacks)
}

func (f *Fake) SimulatePayment(_ context.Context, c byok.Credentials, _ byok.Token, ref string) error {
	f.mu.Lock()
	err := f.enter(byok.OpSimulatePayment)
	ch, ok := f.charges[ref]
	var amount int64
	if ok {
		amount = ch.Amount
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if !c.Sandbox {
		return byok.ErrSandboxOnly
	}
	if !ok {
		return byok.ErrNotFound
	}
	f.Pay(ref, amount, 0, time.Now().UTC())
	return nil
}

func (f *Fake) ParseCallback(_ byok.Product, headers map[string]string, body []byte) (byok.Callback, error) {
	return byok.Callback{Account: headers["x-conta-corrente"], ChargeRefs: []string{string(body)}}, nil
}

// Payout records the payment, once per Ref: a repeated Ref returns the first
// answer, like the bank's idempotency id.
func (f *Fake) Payout(_ context.Context, _ byok.Credentials, _ byok.Token, in byok.PayoutRequest) (byok.Payout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpPayout); err != nil {
		return byok.Payout{}, err
	}
	if p, ok := f.payouts[in.Ref]; ok {
		return *p, nil
	}
	p := &byok.Payout{Ref: in.Ref, ProviderRef: "prov-" + in.Ref, Status: byok.PayoutProcessing}
	f.payouts[in.Ref] = p
	return *p, nil
}

func (f *Fake) GetPayout(_ context.Context, _ byok.Credentials, _ byok.Token, providerRef string) (byok.Payout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(byok.OpGetPayout); err != nil {
		return byok.Payout{}, err
	}
	for _, p := range f.payouts {
		if p.ProviderRef == providerRef {
			return *p, nil
		}
	}
	return byok.Payout{}, byok.ErrNotFound
}

// SettlePayout moves a recorded payout to a final state, as the bank would.
func (f *Fake) SettlePayout(ref string, status byok.PayoutStatus, paymentID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.payouts[ref]; ok {
		p.Status, p.PaymentID, p.Reason = status, paymentID, reason
	}
}

// PayoutCalls is how many times Payout reached the "bank".
func (f *Fake) PayoutCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[byok.OpPayout] }

var _ byok.Payouts = (*Fake)(nil)
