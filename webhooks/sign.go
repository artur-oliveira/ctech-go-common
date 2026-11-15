package webhooks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SignatureScheme prefixes the signature header value.
const SignatureScheme = "v1="

// Sign renders the delivery signature: HMAC-SHA256 over "timestamp.body".
//
// The timestamp is inside the signed material, not merely a header beside it.
// Signing the body alone yields a value that stays valid forever, so anybody
// who captures one delivery can replay it; binding the time lets a consumer
// refuse anything older than their own tolerance.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a "v1=<hex>" signature header against the body and the
// timestamp header, and refuses a timestamp further than tolerance from now.
// It is what a consumer in this family runs, so two implementations of one
// scheme cannot drift.
func Verify(secret, timestamp string, body []byte, signature string, now time.Time, tolerance time.Duration) error {
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("webhooks: timestamp is not a unix time")
	}
	if age := now.Sub(time.Unix(secs, 0)); age > tolerance || age < -tolerance {
		return errors.New("webhooks: timestamp outside the tolerance")
	}
	got, ok := strings.CutPrefix(signature, SignatureScheme)
	if !ok {
		return errors.New("webhooks: unknown signature scheme")
	}
	if !hmac.Equal([]byte(got), []byte(Sign(secret, timestamp, body))) {
		return errors.New("webhooks: signature mismatch")
	}
	return nil
}

// NewSecret returns a signing secret: 256 random bits, hex, prefixed so a
// leaked one is recognisable in a log or a repository scan.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return "whsec_" + hex.EncodeToString(b)
}
