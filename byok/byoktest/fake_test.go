package byoktest

import (
	"context"
	"errors"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

var _ byok.Provider = (*Fake)(nil)

func TestFakeRunsAChargeAndAStatement(t *testing.T) {
	ctx := context.Background()
	f := New()
	c := byok.Credentials{Values: map[string]string{"client_id": "x"}}
	tok, err := f.Token(ctx, c, byok.CapabilityCollection)
	if err != nil {
		t.Fatal(err)
	}
	due, _ := byok.ParseDate("2026-11-10")
	ch, err := f.CreateDueCharge(ctx, c, tok, byok.DueCharge{Ref: "ctb01", Due: due, ValidityDays: 30, Amount: 9990})
	if err != nil || ch.Status != byok.ChargeOpen || ch.Payable == "" {
		t.Fatalf("create = %+v, %v", ch, err)
	}
	again, err := f.CreateDueCharge(ctx, c, tok, byok.DueCharge{Ref: "ctb01", Due: due, Amount: 9990})
	if err != nil || again.Ref != "ctb01" {
		t.Fatal("creating the same ref again is idempotent")
	}
	f.Pay("ctb01", 9990, 0, time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	got, _ := f.GetCharge(ctx, c, tok, "ctb01")
	if got.Status != byok.ChargePaid || len(got.Payments) != 1 {
		t.Fatalf("after Pay = %+v", got)
	}
	f.PageSize = 2
	f.AddLines(
		byok.StatementLine{ID: "1", Date: due, Direction: byok.Credit, Amount: 100},
		byok.StatementLine{ID: "2", Date: due, Direction: byok.Debit, Amount: 50},
		byok.StatementLine{ID: "3", Date: due, Direction: byok.Credit, Amount: 9990, ChargeRef: "ctb01"},
	)
	p1, _ := f.StatementPage(ctx, c, tok, due, due, 0)
	p2, _ := f.StatementPage(ctx, c, tok, due, due, 1)
	if p1.TotalPages != 2 || len(p1.Lines) != 2 || len(p2.Lines) != 1 {
		t.Fatalf("pages = %+v / %+v", p1, p2)
	}
	f.FailNext(byok.OpBalance, byok.ErrUnavailable)
	if _, err := f.Balance(ctx, c, tok, due); !errors.Is(err, byok.ErrUnavailable) {
		t.Fatalf("FailNext not honoured: %v", err)
	}
	if f.Calls(byok.OpBalance) != 1 {
		t.Fatal("calls are counted, failed ones included")
	}
}
