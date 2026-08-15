package jwtverify

import "testing"

func TestJwkToKeyRejectsUnsupportedKty(t *testing.T) {
	if _, _, err := jwkToKey(&jwk{Kid: "x", Kty: "OKP"}); err == nil {
		t.Fatal("jwkToKey accepted an unsupported kty")
	}
}
