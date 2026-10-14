package byoktest

import (
	"context"
	"errors"
	"testing"

	"gopkg.aoctech.app/api-commons/byok"
)

func TestTheFakePaysOncePerRefAndSettlesOnDemand(t *testing.T) {
	f := New()
	ctx := context.Background()
	in := byok.PayoutRequest{Ref: "8bbdede4-35db-4ec9-b652-e176841e62c8", Amount: 100, PixKey: "12345678909"}
	a, _ := f.Payout(ctx, byok.Credentials{}, byok.Token{}, in)
	b, _ := f.Payout(ctx, byok.Credentials{}, byok.Token{}, in)
	if a.ProviderRef != b.ProviderRef || a.Status != byok.PayoutProcessing {
		t.Fatalf("%+v %+v", a, b)
	}
	f.SettlePayout(in.Ref, byok.PayoutDone, "E1", "")
	got, err := f.GetPayout(ctx, byok.Credentials{}, byok.Token{}, a.ProviderRef)
	if err != nil || got.Status != byok.PayoutDone || got.PaymentID != "E1" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := f.GetPayout(ctx, byok.Credentials{}, byok.Token{}, "nope"); !errors.Is(err, byok.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}
