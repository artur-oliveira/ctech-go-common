package fieldcrypt

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testSealer(t *testing.T) *Sealer {
	t.Helper()
	s := NewSealer(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if !s.Enabled() {
		t.Fatalf("test key rejected")
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := testSealer(t)
	const cpf = "529.982.247-25"

	sealed, err := s.Seal(cpf)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "529") {
		t.Fatalf("sealed value still contains the plaintext: %q", sealed)
	}

	got, err := s.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != cpf {
		t.Errorf("Open = %q, want %q", got, cpf)
	}
}

// The nonce is what makes two customers with the same CPF indistinguishable in
// the table. Without it this is deterministic encryption, which leaks equality —
// and equality on a CPF is "these two records are the same person".
func TestSealIsNotDeterministic(t *testing.T) {
	s := testSealer(t)
	a, _ := s.Seal("11144477735")
	b, _ := s.Seal("11144477735")
	if a == b {
		t.Fatalf("two seals of one value produced the same ciphertext")
	}
}

func TestEmptyRoundTripsToEmpty(t *testing.T) {
	s := testSealer(t)
	if got, err := s.Seal(""); err != nil || got != "" {
		t.Errorf("Seal(\"\") = %q, %v", got, err)
	}
	if got, err := s.Open(""); err != nil || got != "" {
		t.Errorf("Open(\"\") = %q, %v", got, err)
	}
}

func TestOpenRefuses(t *testing.T) {
	s := testSealer(t)

	// An unprefixed value is a value this package did not write. Passing it
	// through would make a deployment that stopped sealing look healthy.
	if _, err := s.Open("529.982.247-25"); !errors.Is(err, ErrNotSealed) {
		t.Errorf("Open(plaintext) error = %v, want ErrNotSealed", err)
	}

	sealed, _ := s.Seal("11144477735")
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := s.Open(tampered); err == nil {
		t.Errorf("Open accepted a tampered value")
	}

	other := NewSealer(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if _, err := other.Open(sealed); err == nil {
		t.Errorf("Open accepted a value sealed with a different key")
	}
}

func TestKeyIsRequired(t *testing.T) {
	for name, key := range map[string]string{
		"empty":     "",
		"too short": base64.StdEncoding.EncodeToString([]byte("short")),
		"not a key": "not base64 or hex",
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSealer(key)
			if s.Enabled() {
				t.Fatalf("NewSealer accepted %s", name)
			}
			// The failure has to surface on use, not be swallowed into a
			// pass-through that stores the CPF in the clear.
			if _, err := s.Seal("11144477735"); !errors.Is(err, ErrNoKey) {
				t.Errorf("Seal error = %v, want ErrNoKey", err)
			}
		})
	}
}

// Rotation, end to end: a value sealed by the old key opens under a
// configuration whose write key is the new one, and new values carry the new
// generation. This is the property the whole prefix exists for.
func TestRotationReadsOldAndWritesNew(t *testing.T) {
	old := "0123456789abcdef0123456789abcdef"
	fresh := "fedcba9876543210fedcba9876543210"

	v1 := NewSealer(hexOf(old))
	sealed, err := v1.Seal("12345678909")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "v1.") {
		t.Fatalf("stored value = %q, want a v1 prefix", sealed)
	}

	rotated := NewSealer("2:" + hexOf(fresh) + ",1:" + hexOf(old))
	if got := rotated.WriteGeneration(); got != 2 {
		t.Errorf("WriteGeneration = %d, want 2", got)
	}
	plaintext, err := rotated.Open(sealed)
	if err != nil {
		t.Fatalf("Open of a v1 value under a rotated sealer: %v", err)
	}
	if plaintext != "12345678909" {
		t.Errorf("plaintext = %q", plaintext)
	}

	next, err := rotated.Seal("98765432100")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(next, "v2.") {
		t.Errorf("new value = %q, want a v2 prefix", next)
	}
}

// Dropping the old key too early must say so. "Did not verify" reads as
// tampering and sends whoever is on call looking for an attacker instead of a
// line of configuration.
func TestOpenNamesAMissingGeneration(t *testing.T) {
	sealed, err := NewSealer(hexOf("0123456789abcdef0123456789abcdef")).Seal("12345678909")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	only2 := NewSealer("2:" + hexOf("fedcba9876543210fedcba9876543210"))
	if _, err := only2.Open(sealed); !errors.Is(err, ErrUnknownGeneration) {
		t.Fatalf("err = %v, want ErrUnknownGeneration", err)
	}
}

func TestDuplicateGenerationIsRefused(t *testing.T) {
	s := NewSealer("1:" + hexOf("0123456789abcdef0123456789abcdef") + ",1:" + hexOf("fedcba9876543210fedcba9876543210"))
	if s.Enabled() {
		t.Fatal("two keys on one generation must not build a usable sealer")
	}
}

func hexOf(s string) string { return hex.EncodeToString([]byte(s)) }

func TestSealWithBindsTheValueToItsAssociatedData(t *testing.T) {
	s := testSealer(t)
	sealed, err := s.SealWith("whsec_abc", []byte("webhooks/secret/ORG#1/ENDPOINT#a"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.OpenWith(sealed, []byte("webhooks/secret/ORG#1/ENDPOINT#a")); err != nil || got != "whsec_abc" {
		t.Fatalf("same aad: %q %v", got, err)
	}
	// The same ciphertext moved to another row or field must not open.
	for _, other := range [][]byte{[]byte("webhooks/secret/ORG#1/ENDPOINT#b"), []byte("webhooks/secret/ORG#2/ENDPOINT#a"), nil} {
		if _, err := s.OpenWith(sealed, other); err == nil {
			t.Fatalf("opened with aad %q", other)
		}
	}
}

func TestSealAndOpenStayCompatibleWithNoAssociatedData(t *testing.T) {
	s := testSealer(t)
	sealed, err := s.Seal("x")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.OpenWith(sealed, nil); err != nil || got != "x" {
		t.Fatalf("Seal -> OpenWith(nil): %q %v", got, err)
	}
	withNil, _ := s.SealWith("y", nil)
	if got, err := s.Open(withNil); err != nil || got != "y" {
		t.Fatalf("SealWith(nil) -> Open: %q %v", got, err)
	}
}
