package webpush

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// payloadBudget is the ceiling a product applies to its own payloads (spec: 1 KB), far below MaxPlaintext.
const payloadBudget = 1024

// testdata/notification-v1.json is the golden payload of contract v1. The push-client package copies the
// file and tests the service worker against it, so a change here is a contract change: bump data.v.
func TestGoldenPayloadV1(t *testing.T) {
	raw, err := os.ReadFile("testdata/notification-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		WebPush      int `json:"web_push"`
		Notification struct {
			Title    string `json:"title"`
			Body     string `json:"body"`
			Navigate string `json:"navigate"`
			Tag      string `json:"tag"`
			Lang     string `json:"lang"`
			Silent   bool   `json:"silent"`
			Data     struct {
				V    int    `json:"v"`
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"data"`
		} `json:"notification"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("golden payload has fields outside contract v1: %v", err)
	}
	n := p.Notification
	if p.WebPush != 8030 || n.Data.V != 1 || n.Title == "" || n.Body == "" || n.Tag == "" || n.Lang != "pt-BR" || n.Data.ID == "" || n.Data.Type == "" {
		t.Errorf("golden payload is incomplete: %+v", p)
	}
	if !strings.HasPrefix(n.Navigate, "/") || strings.HasPrefix(n.Navigate, "//") {
		t.Errorf("navigate must be a same-origin path: %q", n.Navigate)
	}
	if len(raw) > payloadBudget {
		t.Errorf("golden payload is %d bytes, budget %d", len(raw), payloadBudget)
	}

	// And it travels: encrypts and a user agent opens exactly these bytes.
	sub, uaPriv := newTestSubscription(t)
	body, err := Encrypt(sub, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decrypt(t, body, uaPriv, sub.Auth); !bytes.Equal(got, raw) {
		t.Error("golden payload does not survive a round trip")
	}
}
