package dataexport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAcker struct {
	mu   sync.Mutex
	acks []Ack
	err  error
}

func (f *fakeAcker) Send(_ context.Context, a Ack) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, a)
	return f.err
}

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func msg(putURL string) Message {
	return Message{Version: Version, Type: TypeExport, RequestID: "r1", Sub: "u1", Service: "poker",
		PutURL: putURL, ExpiresAt: t0.Add(15 * time.Minute), Attempt: 1, IssuedAt: t0}
}

func body(t *testing.T, m Message) []byte {
	t.Helper()
	b, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newHandler(part PartFunc, acks Acker) *Handler {
	h := NewHandler("poker", part, http.DefaultClient, acks)
	h.now = func() time.Time { return t0 }
	h.allow = func(string) bool { return true } // httptest servers are plain http on 127.0.0.1
	return h
}

func TestHandleUploadsThePartAndAcks(t *testing.T) {
	var got []byte
	var ctype string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		ctype = r.Header.Get("Content-Type")
	}))
	defer s3.Close()
	acks := &fakeAcker{}
	h := newHandler(func(_ context.Context, sub string) ([]byte, error) { return []byte("zip-of-" + sub), nil }, acks)

	handled, err := h.Handle(context.Background(), body(t, msg(s3.URL+"/k?sig=SECRET")))
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if string(got) != "zip-of-u1" || ctype != "application/zip" {
		t.Fatalf("uploaded %q as %q", got, ctype)
	}
	if len(acks.acks) != 1 || acks.acks[0].Result != ResultDone || acks.acks[0].Size != 9 || acks.acks[0].RequestID != "r1" || acks.acks[0].Sub != "u1" {
		t.Fatalf("acks=%+v", acks.acks)
	}
}

func TestHandleIgnoresWhatIsNotAnExport(t *testing.T) {
	h := newHandler(nil, &fakeAcker{})
	handled, err := h.Handle(context.Background(), []byte(`{"version":1,"type":"user.erase","request_id":"r"}`))
	if handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
}

func TestHandleAcceptsTheSNSEnvelope(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer s3.Close()
	acks := &fakeAcker{}
	h := newHandler(func(context.Context, string) ([]byte, error) { return []byte("z"), nil }, acks)
	env := `{"Type":"Notification","Message":` + jsonString(string(body(t, msg(s3.URL)))) + `}`
	if handled, err := h.Handle(context.Background(), []byte(env)); !handled || err != nil || len(acks.acks) != 1 {
		t.Fatalf("handled=%v err=%v acks=%d", handled, err, len(acks.acks))
	}
}

func jsonString(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func TestHandleDropsAnotherServicesAndExpiredMessages(t *testing.T) {
	called := false
	h := newHandler(func(context.Context, string) ([]byte, error) { called = true; return nil, nil }, &fakeAcker{})
	other := msg("http://x")
	other.Service = "billing"
	expired := msg("http://x")
	expired.ExpiresAt = t0.Add(-time.Second)
	for _, m := range []Message{other, expired} {
		if handled, err := h.Handle(context.Background(), body(t, m)); !handled || err != nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
	}
	if called {
		t.Fatal("part built for a message that is not for this service or is expired")
	}
}

func TestHandleLeavesFailuresForRedelivery(t *testing.T) {
	s3bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer s3bad.Close()
	ok := func(context.Context, string) ([]byte, error) { return []byte("z"), nil }

	acks := &fakeAcker{}
	if _, err := newHandler(ok, acks).Handle(context.Background(), body(t, msg(s3bad.URL))); err == nil || len(acks.acks) != 0 {
		t.Fatalf("rejected upload must error without acking: err=%v acks=%d", err, len(acks.acks))
	}
	boom := func(context.Context, string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := newHandler(boom, &fakeAcker{}).Handle(context.Background(), body(t, msg("http://x"))); err == nil {
		t.Fatal("part failure must error")
	}
	s3ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer s3ok.Close()
	if _, err := newHandler(ok, &fakeAcker{err: errors.New("ack down")}).Handle(context.Background(), body(t, msg(s3ok.URL))); err == nil {
		t.Fatal("ack failure must error so the upload is repeated")
	}
}

func TestUploadErrorDoesNotLeakThePresignedURL(t *testing.T) {
	_, err := newHandler(func(context.Context, string) ([]byte, error) { return []byte("z"), nil }, &fakeAcker{}).
		Handle(context.Background(), body(t, msg("http://127.0.0.1:1/k?X-Amz-Signature=SECRET")))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err=%v", err)
	}
}

func TestMessageValidation(t *testing.T) {
	for name, mut := range map[string]func(*Message){
		"version": func(m *Message) { m.Version = 9 }, "type": func(m *Message) { m.Type = "x" },
		"request": func(m *Message) { m.RequestID = "" }, "sub": func(m *Message) { m.Sub = "" },
		"service": func(m *Message) { m.Service = "" }, "url": func(m *Message) { m.PutURL = "" },
		"expiry": func(m *Message) { m.ExpiresAt = time.Time{} },
	} {
		m := msg("http://x")
		mut(&m)
		if _, err := Encode(m); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

func TestHandleRefusesAPutURLOutsideHTTPSS3(t *testing.T) {
	called := false
	h := NewHandler("poker", func(context.Context, string) ([]byte, error) { called = true; return nil, nil }, http.DefaultClient, &fakeAcker{})
	h.now = func() time.Time { return t0 }
	for _, u := range []string{"http://b.s3.amazonaws.com/k", "https://evil.example/k", "https://amazonaws.com.evil.example/k", "https://169.254.169.254/k"} {
		if handled, err := h.Handle(context.Background(), body(t, msg(u))); !handled || err != nil {
			t.Fatalf("%s: handled=%v err=%v", u, handled, err)
		}
	}
	if called {
		t.Fatal("a part was built for a refused url")
	}
	if !allowedPutURL("https://b.s3.dualstack.us-east-1.amazonaws.com/exports/k?X-Amz-Signature=s") {
		t.Fatal("dual-stack S3 url must be allowed")
	}
}

func TestUploadDoesNotFollowRedirects(t *testing.T) {
	evilHit := false
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { evilHit = true }))
	defer evil.Close()
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusTemporaryRedirect)
	}))
	defer s3.Close()
	h := newHandler(func(context.Context, string) ([]byte, error) { return []byte("z"), nil }, &fakeAcker{})
	h.http = NewHandler("x", nil, http.DefaultClient, nil).http // the real client, redirects off
	if _, err := h.Handle(context.Background(), body(t, msg(s3.URL))); err == nil || evilHit {
		t.Fatalf("redirect must fail the upload and not be followed: err=%v hit=%v", err, evilHit)
	}
}
