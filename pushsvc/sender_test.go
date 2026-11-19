package pushsvc

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"gopkg.aoctech.app/api-commons/alerts"
	"gopkg.aoctech.app/api-commons/webpush"
)

const testHost = "fcm.googleapis.com"

type fakeStore struct {
	mu        sync.Mutex
	devices   map[string][]Subscription
	deleted   []string
	listErr   error
	deleteErr error
}

func (s *fakeStore) List(_ context.Context, sub string) ([]Subscription, error) {
	return s.devices[sub], s.listErr
}

func (s *fakeStore) Delete(_ context.Context, sub, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, sub+"/"+id)
	return nil
}

type fakeSender struct {
	mu      sync.Mutex
	byToken map[string]webpush.Result
	err     error
	sent    []webpush.Message
}

func (f *fakeSender) Send(_ context.Context, sub webpush.Subscription, _ string, m webpush.Message) (webpush.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	for token, res := range f.byToken {
		if strings.HasSuffix(sub.Endpoint, token) {
			return res, f.err
		}
	}
	return webpush.Result{Status: 201, Outcome: webpush.Delivered}, f.err
}

type recordingAlerts struct {
	mu   sync.Mutex
	jobs []string
}

func (r *recordingAlerts) Alert(_ context.Context, a alerts.Alert) {
	r.mu.Lock()
	r.jobs = append(r.jobs, a.Job)
	r.mu.Unlock()
}

func device(t *testing.T, token string) Subscription {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	endpoint := "https://" + testHost + "/fcm/send/" + token
	return Subscription{
		ID: SubscriptionID(endpoint), Endpoint: endpoint, VapidKID: "kid",
		P256dh: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(auth),
	}
}

func job(recipients ...string) Job {
	return Job{
		EventID: "01EVENT", Type: "document.rejected", Title: "NF-e rejeitada · Acme", Body: "Toque para ver o motivo.",
		Navigate: "/nfe/detail?key=1&org=CNPJ_1", Tag: "document.rejected:ab12", Topic: "topic", TTLSeconds: 3600, Urgency: "high",
		Recipients: recipients,
	}
}

func newHandler(store *fakeStore, sender *fakeSender) (*Handler, *recordingAlerts) {
	a := &recordingAlerts{}
	return NewHandler(store, sender, a), a
}

func TestHandleDeliversToEveryDeviceOfEveryRecipient(t *testing.T) {
	store := &fakeStore{devices: map[string][]Subscription{"a": {device(t, "a1"), device(t, "a2")}, "b": {device(t, "b1")}}}
	sender := &fakeSender{}
	h, al := newHandler(store, sender)
	if err := h.Handle(context.Background(), job("a", "b")); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 3 || len(store.deleted) != 0 || len(al.jobs) != 0 {
		t.Errorf("sent=%d deleted=%v alerts=%v", len(sender.sent), store.deleted, al.jobs)
	}
	m := sender.sent[0]
	if m.TTL != time.Hour || m.Urgency != "high" || m.Topic != "topic" || m.DedupeKey != "01EVENT" || len(m.Payload) == 0 {
		t.Errorf("message = %v", m)
	}
}

func TestHandleDeletesGoneAndUnauthorizedAndAlerts(t *testing.T) {
	gone, bad, ok := device(t, "gone"), device(t, "bad"), device(t, "ok")
	store := &fakeStore{devices: map[string][]Subscription{"a": {gone, bad, ok}}}
	sender := &fakeSender{byToken: map[string]webpush.Result{
		"gone": {Status: 410, Outcome: webpush.Gone}, "bad": {Status: 403, Outcome: webpush.Unauthorized},
	}}
	h, al := newHandler(store, sender)
	if err := h.Handle(context.Background(), job("a")); err != nil {
		t.Fatalf("a refusing device is not a job failure: %v", err)
	}
	if len(store.deleted) != 2 {
		t.Errorf("deleted = %v, want the gone and the unauthorized device", store.deleted)
	}
	if len(al.jobs) != 1 || al.jobs[0] != "push-vapid-rejected" {
		t.Errorf("alerts = %v", al.jobs)
	}
}

func TestHandleAlertsOnATooLargePayloadWithoutFailing(t *testing.T) {
	store := &fakeStore{devices: map[string][]Subscription{"a": {device(t, "big")}}}
	sender := &fakeSender{byToken: map[string]webpush.Result{"big": {Status: 413, Outcome: webpush.PayloadTooLarge}}}
	h, al := newHandler(store, sender)
	if err := h.Handle(context.Background(), job("a")); err != nil {
		t.Fatal(err)
	}
	if len(al.jobs) != 1 || al.jobs[0] != "push-payload-too-large" || len(store.deleted) != 0 {
		t.Errorf("alerts=%v deleted=%v", al.jobs, store.deleted)
	}
}

// Only a failure of the handler's own work (the table) fails the run, so Lambda retries; a transient push-service
// failure is dropped, best effort.
func TestHandleRetriesOnlyJobLevelFailures(t *testing.T) {
	failing := &fakeStore{devices: map[string][]Subscription{}, listErr: errors.New("throttled")}
	h, al := newHandler(failing, &fakeSender{})
	if err := h.Handle(context.Background(), job("a")); err == nil {
		t.Error("an unreadable table must fail the run")
	}
	if len(al.jobs) != 1 || al.jobs[0] != "push-sender-failed" {
		t.Errorf("alerts = %v", al.jobs)
	}

	store := &fakeStore{devices: map[string][]Subscription{"a": {device(t, "slow")}}}
	sender := &fakeSender{byToken: map[string]webpush.Result{"slow": {Outcome: webpush.Transient}}, err: errors.New("webpush: send to " + testHost + ": timeout")}
	h, al = newHandler(store, sender)
	if err := h.Handle(context.Background(), job("a")); err != nil {
		t.Errorf("a transient push-service failure must not fail the run: %v", err)
	}
	if len(al.jobs) != 0 {
		t.Errorf("alerts = %v", al.jobs)
	}

	gone := &fakeStore{devices: map[string][]Subscription{"a": {device(t, "gone")}}, deleteErr: errors.New("throttled")}
	h, _ = newHandler(gone, &fakeSender{byToken: map[string]webpush.Result{"gone": {Status: 410, Outcome: webpush.Gone}}})
	if err := h.Handle(context.Background(), job("a")); err == nil {
		t.Error("a gone device that cannot be deleted must fail the run")
	}
}

func TestHandleDropsAStoredRowThatNoLongerParsesAndRefusesABadJob(t *testing.T) {
	broken := Subscription{ID: "deadbeefdeadbeef", Endpoint: "http://insecure.example/x", P256dh: "x", Auth: "y"}
	store := &fakeStore{devices: map[string][]Subscription{"a": {broken}}}
	sender := &fakeSender{}
	h, _ := newHandler(store, sender)
	if err := h.Handle(context.Background(), job("a")); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || len(sender.sent) != 0 {
		t.Errorf("deleted=%v sent=%d: a row that cannot parse must be dropped, never sent", store.deleted, len(sender.sent))
	}
	bad := job("a")
	bad.Navigate = "https://evil.example/"
	if err := h.Handle(context.Background(), bad); err != nil || len(sender.sent) != 0 {
		t.Errorf("a refused job must not retry or send: %v %d", err, len(sender.sent))
	}
}

type fakeSSM map[string]string

func (f fakeSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	v, ok := f[aws.ToString(in.Name)]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

var paths = Paths{Private: "/p/private", Previous: "/p/previous", Public: "/p/public", Subject: "/p/subject"}

func TestLoadSenderIsQuietWithoutParametersAndStrictAboutMismatch(t *testing.T) {
	ctx := context.Background()
	if _, err := LoadSender(ctx, fakeSSM{}, paths); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no parameters: %v, want ErrNotConfigured (off, not failing)", err)
	}
	pem, key, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ok := fakeSSM{"/p/private": pem, "/p/public": key.PublicBase64URL(), "/p/subject": "https://app.example"}
	if s, err := LoadSender(ctx, ok, paths); err != nil || s == nil {
		t.Fatalf("a complete set must load: %v", err)
	}
	_, other, _ := webpush.GenerateKey()
	mismatch := fakeSSM{"/p/private": pem, "/p/public": other.PublicBase64URL(), "/p/subject": "https://app.example"}
	if _, err := LoadSender(ctx, mismatch, paths); err == nil || errors.Is(err, ErrNotConfigured) {
		t.Errorf("a public key that does not match the private one must fail loudly: %v", err)
	}
	if _, err := LoadSender(ctx, fakeSSM{"/p/private": pem, "/p/public": key.PublicBase64URL()}, paths); err == nil {
		t.Error("a missing subject must fail")
	}
	prevPEM, _, _ := webpush.GenerateKey()
	withPrev := fakeSSM{"/p/private": pem, "/p/public": key.PublicBase64URL(), "/p/subject": "https://app.example", "/p/previous": prevPEM}
	if _, err := LoadSender(ctx, withPrev, paths); err != nil {
		t.Errorf("a previous key must load: %v", err)
	}
}

func TestLambdaNoOpsWhenNotConfiguredAndRetriesLoadAfterATransientError(t *testing.T) {
	// No recipients: nothing is sent once the keys load, so the test never reaches a push service.
	h := NewHandler(&fakeStore{}, nil, &recordingAlerts{})
	run := Lambda(h, fakeSSM{}, paths)
	if err := run(context.Background(), job()); err != nil {
		t.Errorf("without parameters the function stays quiet: %v", err)
	}

	pem, key, _ := webpush.GenerateKey()
	reader := &flakySSM{fakeSSM: fakeSSM{"/p/private": pem, "/p/public": key.PublicBase64URL(), "/p/subject": "https://app.example"}, failFirst: true}
	run = Lambda(NewHandler(&fakeStore{}, nil, &recordingAlerts{}), reader, paths)
	if err := run(context.Background(), job()); err == nil {
		t.Error("a transient SSM failure must fail the invocation")
	}
	if err := run(context.Background(), job()); err != nil {
		t.Errorf("the next invocation must load again: %v", err)
	}
}

type flakySSM struct {
	fakeSSM
	failFirst bool
}

func (f *flakySSM) GetParameter(ctx context.Context, in *ssm.GetParameterInput, o ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if f.failFirst {
		f.failFirst = false
		return nil, errors.New("throttled")
	}
	return f.fakeSSM.GetParameter(ctx, in, o...)
}

func TestLogHelpersNeverCarryTheEndpoint(t *testing.T) {
	endpoint := "https://" + testHost + "/fcm/send/SECRET-TOKEN"
	if got := hostOf(endpoint); got != testHost {
		t.Errorf("hostOf = %q", got)
	}
	if strings.Contains(subHash("user-1"), "user") || len(subHash("user-1")) != 8 {
		t.Errorf("subHash = %q", subHash("user-1"))
	}
}

type fakeLambda struct {
	in     *lambda.InvokeInput
	status int32
	err    error
}

func (f *fakeLambda) Invoke(_ context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	f.in = in
	return &lambda.InvokeOutput{StatusCode: f.status}, f.err
}

func TestInvokerUsesEventInvocation(t *testing.T) {
	f := &fakeLambda{status: 202}
	if err := NewInvoker(f, "push-sender").Send(context.Background(), job("a")); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(f.in.FunctionName) != "push-sender" || string(f.in.InvocationType) != "Event" || !strings.Contains(string(f.in.Payload), `"event_id":"01EVENT"`) {
		t.Errorf("input = %+v", f.in)
	}
	if err := NewInvoker(&fakeLambda{status: 500}, "f").Send(context.Background(), job("a")); err == nil {
		t.Error("a non-202 async invoke must be an error")
	}
	if err := NewInvoker(&fakeLambda{err: errors.New("boom")}, "f").Send(context.Background(), job("a")); err == nil {
		t.Error("an invoke error must be returned")
	}
}
