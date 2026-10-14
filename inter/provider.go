package inter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
	"gopkg.aoctech.app/api-commons/inter/pix"
)

// Provider adapts Client to byok.Provider. Plaintext Values are required
// (byok.Credentials.Ref is opened by the caller, never here).
type Provider struct{ c *Client }

func NewProvider(c *Client) *Provider { return &Provider{c: c} }

func (p *Provider) Name() string { return "inter" }

func (p *Provider) CredentialFields() []byok.Field {
	return []byok.Field{
		{Name: "client_id", Required: true},
		{Name: "client_secret", Secret: true, Required: true},
		{Name: "cert_pem", File: true, Required: true},
		{Name: "key_pem", File: true, Secret: true, Required: true},
	}
}

func scopesFor(set byok.Capability) []string {
	if set == byok.CapabilityCollection {
		return ScopesCollection
	}
	for _, o := range optionalProducts {
		if o.capability == set {
			return o.scopes
		}
	}
	return ScopesStatement
}

func (p *Provider) creds(c byok.Credentials) (Credentials, error) { return CredentialsFrom(c) }

func (p *Provider) Connect(ctx context.Context, c byok.Credentials) (byok.Connected, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Connected{}, err
	}
	info, err := InspectPair(cr.CertPEM, cr.KeyPEM, time.Now())
	if err != nil {
		return byok.Connected{}, err
	}
	// Everything the integration may be used for in one request; Inter refuses the
	// whole request when one scope is not ticked, so a refusal steps down: first
	// to statement + collection (the original five activities), then to statement
	// only (D23). Only a refusal steps down: a transient failure (5xx, timeout,
	// rate limit) must not store a fully-scoped integration as a smaller one.
	// This token is used for the balance check below and thrown away; every real
	// call gets a token of its own product's scopes (Token).
	everything := append(slices.Clone(ScopesStatement), ScopesCollection...)
	for _, o := range optionalProducts {
		everything = append(everything, o.scopes...)
	}
	ladder := [][]string{everything,
		append(slices.Clone(ScopesStatement), ScopesCollection...), ScopesStatement}
	var (
		tok   byok.Token
		asked []string
	)
	for i, rung := range ladder {
		asked = rung
		if tok, err = p.c.FetchToken(ctx, cr, asked); err == nil {
			break
		}
		if i == len(ladder)-1 || (!errors.Is(err, byok.ErrCredentialsRefused) && !errors.Is(err, byok.ErrScopesRefused) && !errors.Is(err, byok.ErrInvalidRequest)) {
			return byok.Connected{}, err
		}
	}
	granted := tok.Scopes
	if len(granted) == 0 { // Inter echoed no scope: it granted what was asked
		granted = asked
	}
	out := byok.Connected{ScopesGranted: granted, CertSubject: info.Subject, CertFingerprint: info.Fingerprint, CertNotAfter: info.NotAfter}
	if slices.Contains(granted, "extrato.read") {
		out.Capabilities = append(out.Capabilities, byok.CapabilityStatement)
	}
	for _, s := range ScopesCollection {
		if !slices.Contains(granted, s) {
			out.MissingScopes = append(out.MissingScopes, s)
		}
	}
	if len(out.MissingScopes) == 0 {
		out.Capabilities = append(out.Capabilities, byok.CapabilityCollection)
	}
	for _, o := range optionalProducts {
		if !slices.ContainsFunc(o.scopes, func(sc string) bool { return !slices.Contains(granted, sc) }) {
			out.Capabilities = append(out.Capabilities, o.capability)
		}
	}
	if !slices.Contains(out.Capabilities, byok.CapabilityStatement) {
		return byok.Connected{}, fmt.Errorf("%w: extrato.read is required", byok.ErrScopesRefused)
	}
	// GET /saldo proves extrato.read and the account number together.
	bal, err := p.Balance(ctx, c, byok.Token{Value: tok.Value}, byok.Date{})
	if err != nil {
		return byok.Connected{}, err
	}
	out.Balance = bal
	return out, nil
}

func (p *Provider) Token(ctx context.Context, c byok.Credentials, set byok.Capability) (byok.Token, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Token{}, err
	}
	tok, err := p.c.FetchToken(ctx, cr, scopesFor(set))
	if err != nil {
		return byok.Token{}, err
	}
	if info, ierr := InspectPair(cr.CertPEM, cr.KeyPEM, time.Now()); ierr == nil {
		tok.CertNotAfter = info.NotAfter
	}
	return tok, nil
}

func basisPoints(bp int) string { return fmt.Sprintf("%d.%02d", bp/100, bp%100) }

func (p *Provider) CreateDueCharge(ctx context.Context, c byok.Credentials, t byok.Token, in byok.DueCharge) (byok.Charge, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Charge{}, err
	}
	dev := pix.Devedor{Nome: in.Debtor.Name}
	switch len(in.Debtor.TaxID) {
	case 11:
		dev.CPF = in.Debtor.TaxID
	case 14:
		dev.CNPJ = in.Debtor.TaxID
	default:
		return byok.Charge{}, fmt.Errorf("%w: debtor tax id", byok.ErrInvalidRequest)
	}
	req := pix.CobvRequest{
		Calendario: pix.Calendario{DataDeVencimento: in.Due.String(), ValidadeAposVencimento: max(in.ValidityDays, 1)},
		Devedor:    dev, Valor: pix.Valor{Original: Reais(in.Amount)}, Chave: cr.PixKey, SolicitacaoPagador: in.Message,
	}
	if in.FineBasisPoints > 0 {
		req.Valor.Multa = &pix.Modalidade{Modalidade: 2, ValorPerc: basisPoints(in.FineBasisPoints)}
	}
	if in.MonthlyInterestBasisPoints > 0 {
		req.Valor.Juros = &pix.Modalidade{Modalidade: 3, ValorPerc: basisPoints(in.MonthlyInterestBasisPoints)}
	}
	got, err := p.c.PutCobv(ctx, cr, t.Value, in.Ref, req)
	if err != nil {
		// PUT on an existing txid: read it back (idempotent create, spec § 7.2).
		if existing, gerr := p.c.GetCobv(ctx, cr, t.Value, in.Ref); gerr == nil {
			return chargeOf(existing)
		}
		return byok.Charge{}, err
	}
	return chargeOf(got)
}

func cents(s string) int64 { v, _ := Cents(s); return v }

// chargeOf maps a cobv. An unknown status (an empty body, a status Inter adds
// later) is an error, never "removed": a caller must not fail a live charge.
func chargeOf(v pix.Cobv) (byok.Charge, error) {
	ch := byok.Charge{Ref: v.Txid, Amount: cents(v.Valor.Original), Payable: v.PixCopiaECola}
	switch v.Status {
	case pix.StatusAtiva:
		ch.Status = byok.ChargeOpen
	case pix.StatusConcluida:
		ch.Status = byok.ChargePaid
	case pix.StatusRemovidaPSP:
		ch.Status = byok.ChargeExpired
	case pix.StatusRemovidaRecebedor:
		ch.Status = byok.ChargeRemoved
	default:
		return byok.Charge{}, fmt.Errorf("%w: cobv status %q", byok.ErrUnavailable, v.Status)
	}
	for _, r := range v.Pix {
		pay := byok.Payment{ID: r.EndToEndID, Amount: cents(r.Valor)}
		if t, err := time.Parse(time.RFC3339, r.Horario); err == nil {
			pay.PaidAt = t.UTC()
		}
		if cv := r.ComponentesValor; cv != nil {
			if cv.Multa != nil {
				pay.LateCharges += cents(cv.Multa.Valor)
			}
			if cv.Juros != nil {
				pay.LateCharges += cents(cv.Juros.Valor)
			}
		}
		ch.Payments = append(ch.Payments, pay)
	}
	if len(ch.Payments) > 0 {
		ch.Status = byok.ChargePaid
	}
	return ch, nil
}

func (p *Provider) GetCharge(ctx context.Context, c byok.Credentials, t byok.Token, ref string) (byok.Charge, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Charge{}, err
	}
	v, err := p.c.GetCobv(ctx, cr, t.Value, ref)
	if err != nil {
		return byok.Charge{}, err
	}
	return chargeOf(v)
}

func (p *Provider) UpdateCharge(ctx context.Context, c byok.Credentials, t byok.Token, ref string, amount int64) (byok.Charge, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Charge{}, err
	}
	v, err := p.c.PatchCobvAmount(ctx, cr, t.Value, ref, amount)
	if err != nil {
		return byok.Charge{}, err
	}
	return chargeOf(v)
}

func (p *Provider) CancelCharge(ctx context.Context, c byok.Credentials, t byok.Token, ref string) error {
	cr, err := p.creds(c)
	if err != nil {
		return err
	}
	return p.c.RemoveCobv(ctx, cr, t.Value, ref)
}

func refundOf(d pix.Devolucao, paymentID string) byok.Refund {
	r := byok.Refund{ID: d.ID, PaymentID: paymentID, Amount: cents(d.Valor), Status: byok.RefundProcessing}
	switch d.Status {
	case "DEVOLVIDO":
		r.Status = byok.RefundDone
	case "NAO_REALIZADO":
		r.Status = byok.RefundFailed
	}
	return r
}

func (p *Provider) Refund(ctx context.Context, c byok.Credentials, t byok.Token, in byok.RefundRequest) (byok.Refund, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Refund{}, err
	}
	d, err := p.c.PutDevolucao(ctx, cr, t.Value, in.PaymentID, in.RefundID, in.Amount, in.Reason)
	if err != nil {
		return byok.Refund{}, err
	}
	return refundOf(d, in.PaymentID), nil
}

func (p *Provider) GetRefund(ctx context.Context, c byok.Credentials, t byok.Token, paymentID, refundID string) (byok.Refund, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Refund{}, err
	}
	d, err := p.c.GetDevolucao(ctx, cr, t.Value, paymentID, refundID)
	if err != nil {
		return byok.Refund{}, err
	}
	return refundOf(d, paymentID), nil
}

const statementPageSize = 1000

func (p *Provider) StatementPage(ctx context.Context, c byok.Credentials, t byok.Token, from, to byok.Date, page int) (byok.StatementPage, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.StatementPage{}, err
	}
	lines, total, err := p.c.ExtratoCompleto(ctx, cr, t.Value, from, to, page, statementPageSize)
	if err != nil {
		return byok.StatementPage{}, err
	}
	out := byok.StatementPage{Page: page, TotalPages: total}
	for _, l := range lines {
		d, err := byok.ParseDate(l.DataTransacao[:min(10, len(l.DataTransacao))])
		if err != nil {
			continue // unreadable line: the balance difference banner shows it (spec § 8.1)
		}
		// An unreadable amount or direction is skipped like an unreadable date:
		// posting it as zero would consume the line's lock with a wrong entry.
		amount, aerr := Cents(l.Valor)
		var dir byok.Direction
		switch strings.ToUpper(l.TipoOperacao) {
		case "C":
			dir = byok.Credit
		case "D":
			dir = byok.Debit
		}
		if aerr != nil || dir == "" || l.IDTransacao == "" {
			continue
		}
		line := byok.StatementLine{ID: l.IDTransacao, Date: d, Direction: dir, Amount: amount,
			Title: l.Titulo, Description: l.Descricao, ChargeRef: l.Detalhes.TxID, PaymentID: l.Detalhes.EndToEndID}
		// Only a credit has a payer: on a debit the "pagador" is the account's own holder.
		if dir == byok.Credit && (l.Detalhes.NomePagador != "" || l.Detalhes.CPFCNPJPagador != "") {
			line.Payer = &byok.Payer{Name: l.Detalhes.NomePagador, Document: l.Detalhes.CPFCNPJPagador, Institution: l.Detalhes.NomeEmpresaPagador}
		}
		out.Lines = append(out.Lines, line)
	}
	return out, nil
}

func reaisFloat(v float64) int64 { return int64(math.Round(v * 100)) }

func (p *Provider) Balance(ctx context.Context, c byok.Credentials, t byok.Token, at byok.Date) (byok.Balance, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Balance{}, err
	}
	s, err := p.c.Saldo(ctx, cr, t.Value, at)
	if err != nil {
		return byok.Balance{}, err
	}
	return byok.Balance{Available: reaisFloat(s.Disponivel),
		Blocked: reaisFloat(s.BloqueadoCheque + s.BloqueadoJudicialmente + s.BloqueadoAdministrativo), At: at}, nil
}

func pixOnly(product byok.Product) error {
	if product != byok.ProductPix {
		return fmt.Errorf("%w: product %q", byok.ErrInvalidRequest, product)
	}
	return nil
}

func (p *Provider) GetWebhook(ctx context.Context, c byok.Credentials, t byok.Token, product byok.Product) (string, error) {
	if err := pixOnly(product); err != nil {
		return "", err
	}
	cr, err := p.creds(c)
	if err != nil {
		return "", err
	}
	return p.c.GetWebhookURL(ctx, cr, t.Value)
}

func (p *Provider) RegisterWebhook(ctx context.Context, c byok.Credentials, t byok.Token, product byok.Product, url string) error {
	if err := pixOnly(product); err != nil {
		return err
	}
	cr, err := p.creds(c)
	if err != nil {
		return err
	}
	return p.c.PutWebhookURL(ctx, cr, t.Value, url)
}

func (p *Provider) DeleteWebhook(ctx context.Context, c byok.Credentials, t byok.Token, product byok.Product) error {
	if err := pixOnly(product); err != nil {
		return err
	}
	cr, err := p.creds(c)
	if err != nil {
		return err
	}
	return p.c.DeleteWebhookURL(ctx, cr, t.Value)
}

func (p *Provider) ListCallbacks(ctx context.Context, c byok.Credentials, t byok.Token, since time.Time) ([]byok.CallbackAttempt, error) {
	cr, err := p.creds(c)
	if err != nil {
		return nil, err
	}
	recs, err := p.c.ListCallbacks(ctx, cr, t.Value, since, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]byok.CallbackAttempt, 0, len(recs))
	for _, r := range recs {
		at, _ := time.Parse(time.RFC3339, r.DataHoraDisparo)
		// The record names its charges inside the payload it delivered
		// (pix[].txid); a top-level txid is only a fallback.
		refs := []string{}
		var body pix.CallbackBody
		if json.Unmarshal(r.Payload, &body) == nil {
			for _, x := range body.Pix {
				if x.Txid != "" {
					refs = append(refs, x.Txid)
				}
			}
		}
		if len(refs) == 0 && r.Txid != "" {
			refs = append(refs, r.Txid)
		}
		for _, ref := range refs {
			out = append(out, byok.CallbackAttempt{ChargeRef: ref, HTTPStatus: r.HTTPStatus, Attempt: r.NumeroTentativa, At: at.UTC()})
		}
		if len(refs) == 0 {
			out = append(out, byok.CallbackAttempt{HTTPStatus: r.HTTPStatus, Attempt: r.NumeroTentativa, At: at.UTC()})
		}
	}
	return out, nil
}

func (p *Provider) RetryCallbacks(ctx context.Context, c byok.Credentials, t byok.Token, refs []string) error {
	cr, err := p.creds(c)
	if err != nil {
		return err
	}
	refs = slices.DeleteFunc(slices.Clone(refs), func(r string) bool { return r == "" })
	for start := 0; start < len(refs); start += maxRetryTxids {
		if err := p.c.RetryCallbacks(ctx, cr, t.Value, refs[start:min(start+maxRetryTxids, len(refs))]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) SimulatePayment(ctx context.Context, c byok.Credentials, t byok.Token, ref string) error {
	cr, err := p.creds(c)
	if err != nil {
		return err
	}
	return p.c.PaySandboxCobv(ctx, cr, t.Value, ref)
}

func jsonNumber(s string) json.Number { return json.Number(s) }
