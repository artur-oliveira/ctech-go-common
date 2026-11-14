package jwtverify

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
)

func rsaJWK(t *testing.T, kid string) jwk {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return jwk{Kid: kid, Kty: "RSA",
		N: base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())}
}

func TestJwkToKeyRejectsUnsupportedKty(t *testing.T) {
	if _, _, err := jwkToKey(&jwk{Kid: "x", Kty: "OKP"}); err == nil {
		t.Fatal("jwkToKey accepted an unsupported kty")
	}
}

// A provider with two instances can answer with different key sets (one late to
// reload after a rotation, prod 2026-11-13). A kid already seen must survive a
// fetch that lands on the lagging instance, but only for keyCarryOver.
func TestStoreKeepsRecentlySeenKeysAbsentFromALaterFetch(t *testing.T) {
	v := NewVerifier("http://unused", "", "", cache.NewMemoryBackend(1))
	oldK, newK := rsaJWK(t, "kid-old"), rsaJWK(t, "kid-new")

	v.store([]jwk{newK, oldK})  // rotated instance
	set := v.store([]jwk{oldK}) // lagging instance
	if _, ok := set.byKID["kid-new"]; !ok {
		t.Fatal("kid-new dropped by a stale JWKS response")
	}

	// Past the window the key is really gone.
	e := set.byKID["kid-new"]
	e.seen = time.Now().Add(-keyCarryOver - time.Second)
	set.byKID["kid-new"] = e
	if _, ok := v.store([]jwk{oldK}).byKID["kid-new"]; ok {
		t.Fatal("kid-new kept past keyCarryOver")
	}
}
