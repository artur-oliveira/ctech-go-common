package qr

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestPNGIsAPNG(t *testing.T) {
	b64, err := PNG("00020126580014br.gov.bcb.pix")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(b64)
	if !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		t.Fatal("not a PNG")
	}
}
