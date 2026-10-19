package dynamo

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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	targetGetItem    = "DynamoDB_20120810.GetItem"
	targetUpdateItem = "DynamoDB_20120810.UpdateItem"
	okBody           = `{}`
	conditionFailed  = `{"__type":"com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException","message":"x"}`
	txConflict       = `{"__type":"com.amazonaws.dynamodb.v20120810#TransactionConflictException","message":"x"}`
)

type call struct{ target, body string }

// fakeDynamo answers each request with the next scripted (status, body) for
// its operation and records every call.
type fakeDynamo struct {
	mu      sync.Mutex
	calls   []call
	scripts map[string][]string // target -> bodies; 400 when the body has __type
}

func (f *fakeDynamo) count(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.target == target {
			n++
		}
	}
	return n
}

func newFakeBase(t *testing.T, scripts map[string][]string) (*Base, *fakeDynamo) {
	t.Helper()
	f := &fakeDynamo{scripts: scripts}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		target := r.Header.Get("X-Amz-Target")
		f.mu.Lock()
		f.calls = append(f.calls, call{target, string(body)})
		queue := f.scripts[target]
		resp := okBody
		if len(queue) > 0 {
			resp, f.scripts[target] = queue[0], queue[1:]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		if strings.Contains(resp, "__type") {
			w.WriteHeader(http.StatusBadRequest)
		}
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	db := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		Credentials:  credentials.NewStaticCredentialsProvider("a", "b", ""),
		Retryer:      aws.NopRetryer{},
	})
	b := NewBase(db, "t", "items")
	return &b, f
}

func itemWithVersion(v string) string {
	if v == "" {
		return `{"Item":{"pk":{"S":"p"},"n":{"N":"1"}}}`
	}
	return `{"Item":{"pk":{"S":"p"},"n":{"N":"1"},"version":{"N":"` + v + `"}}}`
}

func TestConditionalUpdate(t *testing.T) {
	cond := map[string]types.AttributeValue{":want": &types.AttributeValueMemberS{Value: "x"}}
	for _, tc := range []struct {
		name      string
		script    []string
		wantOK    bool
		wantErr   bool
		wantCalls int
	}{
		{"applied", nil, true, false, 1},
		{"condition failed is a verdict, never retried", []string{conditionFailed, okBody}, false, false, 1},
		{"transient conflict is retried", []string{txConflict, okBody}, true, false, 2},
		{"conflict beyond the cap fails", []string{txConflict, txConflict, txConflict, okBody}, false, true, DefaultMaxAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := newFakeBase(t, map[string][]string{targetUpdateItem: tc.script})
			ok, err := b.ConditionalUpdate(context.Background(), "p", nil,
				map[string]any{"n": 2}, "#st = :want", map[string]string{"#st": "status"}, cond)
			if ok != tc.wantOK || (err != nil) != tc.wantErr {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if got := f.count(targetUpdateItem); got != tc.wantCalls {
				t.Errorf("calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestConditionalUpdateRejectsTokenClash(t *testing.T) {
	b, f := newFakeBase(t, nil)
	_, err := b.ConditionalUpdate(context.Background(), "p", nil, map[string]any{"n": 2}, "n = :n", nil,
		map[string]types.AttributeValue{":n": &types.AttributeValueMemberN{Value: "1"}})
	if err == nil || f.count(targetUpdateItem) != 0 {
		t.Fatalf("want clash error before any call, got %v", err)
	}
	_, err = b.ConditionalUpdate(context.Background(), "p", nil, map[string]any{"n": 2}, "#n = :x",
		map[string]string{"#n": "other"}, nil)
	if err == nil {
		t.Fatal("want name clash error")
	}
}

func TestConditionalUpdateHonorsContextDuringBackoff(t *testing.T) {
	b, _ := newFakeBase(t, map[string][]string{targetUpdateItem: {txConflict, txConflict}})
	ctx, cancel := context.WithTimeout(context.Background(), retryBackoff/4)
	defer cancel()
	start := time.Now()
	if _, err := b.ConditionalUpdate(ctx, "p", nil, map[string]any{"n": 2}, "attribute_exists(pk)", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > retryBackoff*3 {
		t.Error("did not stop at the deadline")
	}
}

func TestUpdateVersioned(t *testing.T) {
	mutate := func(map[string]types.AttributeValue) (map[string]any, error) { return map[string]any{"n": 2}, nil }
	errBusiness := errors.New("not pending")
	for _, tc := range []struct {
		name       string
		scripts    map[string][]string
		mutate     func(map[string]types.AttributeValue) (map[string]any, error)
		attempts   int
		wantErr    error
		wantGets   int
		wantWrites int
	}{
		{"first try", map[string][]string{targetGetItem: {itemWithVersion("4")}}, mutate, 0, nil, 1, 1},
		{"lost race re-reads and wins", map[string][]string{
			targetGetItem: {itemWithVersion("4"), itemWithVersion("5")}, targetUpdateItem: {conditionFailed, okBody}}, mutate, 0, nil, 2, 2},
		{"exhausted", map[string][]string{
			targetGetItem: {itemWithVersion("1"), itemWithVersion("2")}, targetUpdateItem: {conditionFailed, conditionFailed}}, mutate, 2, ErrVersionConflict, 2, 2},
		{"missing item", map[string][]string{targetGetItem: {okBody}}, mutate, 0, ErrItemNotFound, 1, 0},
		{"mutate error is final", map[string][]string{targetGetItem: {itemWithVersion("1")}},
			func(map[string]types.AttributeValue) (map[string]any, error) { return nil, errBusiness }, 0, errBusiness, 1, 0},
		{"no updates is a no-op", map[string][]string{targetGetItem: {itemWithVersion("1")}},
			func(map[string]types.AttributeValue) (map[string]any, error) { return nil, nil }, 0, nil, 1, 0},
		{"unversioned item", map[string][]string{targetGetItem: {itemWithVersion("")}}, mutate, 0, nil, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := newFakeBase(t, tc.scripts)
			err := b.UpdateVersioned(context.Background(), "p", nil, tc.mutate, tc.attempts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if g, w := f.count(targetGetItem), tc.wantGets; g != w {
				t.Errorf("gets = %d, want %d", g, w)
			}
			if g, w := f.count(targetUpdateItem), tc.wantWrites; g != w {
				t.Errorf("writes = %d, want %d", g, w)
			}
		})
	}
}

func TestUpdateVersionedConditionAndBump(t *testing.T) {
	for _, tc := range []struct {
		name, item, wantCond, wantVersion string
	}{
		{"versioned", itemWithVersion("4"), "#version = :expected_version", `"N":"5"`},
		{"unversioned", itemWithVersion(""), "attribute_exists(pk) AND attribute_not_exists(#version)", `"N":"1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := newFakeBase(t, map[string][]string{targetGetItem: {tc.item}})
			err := b.UpdateVersioned(context.Background(), "p", nil, func(map[string]types.AttributeValue) (map[string]any, error) {
				return map[string]any{"n": 2}, nil
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var body string
			for _, c := range f.calls {
				if c.target == targetUpdateItem {
					body = c.body
				}
			}
			if !strings.Contains(body, tc.wantCond) || !strings.Contains(body, tc.wantVersion) {
				t.Errorf("update body = %s", body)
			}
		})
	}
}
