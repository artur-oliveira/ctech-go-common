package erasure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/oauth2client"
)

// ackServer fakes ctech-account: a client_credentials token endpoint and the ack endpoint.
type ackServer struct {
	srv    *httptest.Server
	status atomic.Int64
	mu     sync.Mutex
	acks   []Ack
	auth   []string
}

func newAckServer(t *testing.T) *ackServer {
	t.Helper()
	a := &ackServer{}
	a.status.Store(http.StatusNoContent)
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/ack", func(w http.ResponseWriter, r *http.Request) {
		code := int(a.status.Load())
		if code/100 == 2 {
			var ack Ack
			_ = json.NewDecoder(r.Body).Decode(&ack)
			a.mu.Lock()
			a.acks = append(a.acks, ack)
			a.auth = append(a.auth, r.Header.Get("Authorization"))
			a.mu.Unlock()
		}
		w.WriteHeader(code)
		if code/100 != 2 {
			_, _ = w.Write([]byte(`{"title":"boom"}`))
		}
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *ackServer) client() *AckClient {
	tm := oauth2client.New(a.srv.Client(), nil, a.srv.URL+"/token", "dfe", "secret", "account:erasure:ack")
	return NewAckClient(a.srv.Client(), a.srv.URL+"/ack", tm)
}

func (a *ackServer) received() []Ack {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Ack(nil), a.acks...)
}

func TestAckClient_SendsBearerJSON(t *testing.T) {
	a := newAckServer(t)
	in := Ack{RequestID: "r1", Service: "dfe", Result: ResultDone, Counts: map[string]int{"users": 1}, At: t0}
	if err := a.client().Send(context.Background(), in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := a.received()
	if len(got) != 1 || got[0].RequestID != "r1" || got[0].Counts["users"] != 1 || !got[0].At.Equal(t0) {
		t.Fatalf("unexpected acks: %+v", got)
	}
	if a.auth[0] != "Bearer tok" {
		t.Fatalf("Authorization = %q", a.auth[0])
	}
}

func TestAckClient_Non2xxIsError(t *testing.T) {
	a := newAckServer(t)
	a.status.Store(http.StatusInternalServerError)
	err := a.client().Send(context.Background(), Ack{RequestID: "r1", Service: "dfe", Result: ResultDone, At: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want a 500 error", err)
	}
}
