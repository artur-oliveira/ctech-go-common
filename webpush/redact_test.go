package webpush

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/resilient"
)

const secretPath = "/push/SECRETPATHTOKEN-9f3a"

// secretsOf lists what must never reach an error, a log line or a printed value: the endpoint (its path is
// a per-subscription token), the keys, the encrypted body marker and anything that looks like the VAPID header.
func secretsOf(sub Subscription, endpoint string) []string {
	return []string{
		"SECRETPATHTOKEN", endpoint, strings.TrimPrefix(endpoint, "https://"),
		base64.RawURLEncoding.EncodeToString(sub.Auth),
		base64.RawURLEncoding.EncodeToString(sub.P256dh),
		"vapid t=", "eyJ", "Bearer",
	}
}

func assertClean(t *testing.T, what, got string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(got, s) {
			t.Errorf("%s leaks %q: %s", what, s, got)
		}
	}
}

// Every failure path of Send: network error, timeout, open circuit, a redirect refused by the client,
// the refusal of the guarded client, plus what is printed for the Result.
func TestSendErrorsNeverCarryEndpointKeysOrJWT(t *testing.T) {
	ctx := context.Background()

	t.Run("network error", func(t *testing.T) {
		s, sub, key, srv := newTarget(t, func(w http.ResponseWriter, r *http.Request) {})
		srv.Close()
		res, err := s.Send(ctx, sub, key.KID, msg())
		if err == nil {
			t.Fatal("want an error from a closed server")
		}
		sec := secretsOf(sub, sub.Endpoint)
		assertClean(t, "error", err.Error(), sec)
		assertClean(t, "error %+v", fmt.Sprintf("%+v", err), sec)
		assertClean(t, "result", fmt.Sprintf("%+v %v %#v", res, res, res), sec)
		if !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("error should still name the host: %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := s.Send(tctx, sub, key.KID, msg())
		if err == nil {
			t.Fatal("want a timeout")
		}
		assertClean(t, "timeout error", err.Error(), secretsOf(sub, sub.Endpoint))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the cause must stay reachable with errors.Is: %v", err)
		}
	})

	t.Run("open circuit", func(t *testing.T) {
		s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) },
			resilient.WithMaxAttempts(1), resilient.WithBreaker(resilient.Config{Failures: 1, Cooldown: time.Minute}))
		var err error
		for i := 0; i < 3; i++ {
			_, err = s.Send(ctx, sub, key.KID, msg())
		}
		if !errors.Is(err, resilient.ErrCircuitOpen) {
			t.Fatalf("want ErrCircuitOpen reachable, got %v", err)
		}
		assertClean(t, "circuit error", err.Error(), secretsOf(sub, sub.Endpoint))
	})

	t.Run("redirect refused by the client", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere/SECRETPATHTOKEN-9f3a", http.StatusFound)
		}))
		defer srv.Close()
		_, key, _ := GenerateKey()
		sub, _ := newTestSubscription(t)
		sub.Endpoint = srv.URL + secretPath
		c := srv.Client()
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
		_, err := NewSender(c, "mailto:ops@example.com", key).Send(ctx, sub, key.KID, msg())
		if err == nil {
			t.Fatal("want the redirect refusal as an error")
		}
		assertClean(t, "redirect error", err.Error(), secretsOf(sub, sub.Endpoint))
	})

	t.Run("guarded client refuses loopback", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		_, key, _ := GenerateKey()
		sub, _ := newTestSubscription(t)
		sub.Endpoint = srv.URL + secretPath
		_, err := NewSender(NewClient(), "mailto:ops@example.com", key).Send(ctx, sub, key.KID, msg())
		if err == nil {
			t.Fatal("want a dial refusal")
		}
		assertClean(t, "guard error", err.Error(), secretsOf(sub, sub.Endpoint))
	})

	t.Run("a refusal by the push service is a Result", func(t *testing.T) {
		s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(410) })
		res, err := s.Send(ctx, sub, key.KID, msg())
		if err != nil {
			t.Fatal(err)
		}
		assertClean(t, "result", fmt.Sprintf("%+v %v %#v", res, res, res), secretsOf(sub, sub.Endpoint))
	})
}

// Validation errors name no value: not the endpoint, the keys, nor the payload.
func TestValidationErrorsCarryNoValues(t *testing.T) {
	p, a := validKeys(t)
	endpoint := "https://fcm.googleapis.com" + secretPath
	cases := map[string][3]string{
		"bad p256dh": {endpoint, "AAAA", a},
		"bad auth":   {endpoint, p, "AAAA"},
		"bad host":   {"https://evil.example.com" + secretPath, p, a},
		"odd port":   {"https://fcm.googleapis.com:8443" + secretPath, p, a},
		"http":       {"http://fcm.googleapis.com" + secretPath, p, a},
	}
	for name, c := range cases {
		_, err := ParseSubscription(c[0], c[1], c[2])
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		assertClean(t, name, err.Error(), []string{"SECRETPATHTOKEN", p, a})
	}

	_, key, _ := GenerateKey()
	if _, err := VAPID(key, "https://x"+secretPath+"\x7f", "mailto:a@b.c", time.Now()); err != nil {
		assertClean(t, "VAPID error", err.Error(), []string{"SECRETPATHTOKEN"})
	}
	if _, err := ParseKey("-----BEGIN PRIVATE KEY-----\nQUJDREVGRw==\n-----END PRIVATE KEY-----"); err != nil {
		assertClean(t, "ParseKey error", err.Error(), []string{"QUJDREVGRw"})
	}
}

// Subscription, Key and Message print redacted through fmt and slog.
func TestPublicTypesPrintRedacted(t *testing.T) {
	sub, _ := newTestSubscription(t)
	sub.Endpoint = "https://fcm.googleapis.com" + secretPath
	_, key, _ := GenerateKey()
	m := Message{Payload: []byte("SECRETPAYLOAD title"), TTL: time.Minute, DedupeKey: "evt-SECRETKEY"}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("send", "sub", sub, "key", key, "msg", m, "subptr", &sub)
	out := buf.String()
	for _, v := range []any{sub, &sub, key, &key, m, &m} {
		out += fmt.Sprintf("|%v|%+v|%#v|%s", v, v, v, v)
	}
	out += fmt.Sprint(struct {
		S Subscription
		K Key
		M Message
	}{sub, key, m})

	sec := append(secretsOf(sub, sub.Endpoint), "SECRETPAYLOAD", "SECRETKEY", key.PublicBase64URL())
	assertClean(t, "printed values", out, sec)
	if !strings.Contains(out, "fcm.googleapis.com") || !strings.Contains(out, key.KID) {
		t.Errorf("host and KID should still identify the value: %s", out)
	}
}
