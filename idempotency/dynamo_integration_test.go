//go:build integration

// Integration tests for DynamoStore against DynamoDB Local.
//
//	docker compose -f docker-compose.test.yml up -d
//	DYNAMODB_ENDPOINT=http://localhost:8131 go test -tags integration -race -count=1 ./idempotency/
package idempotency_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/dynamo"
	"gopkg.aoctech.app/api-commons/idempotency"
)

func localStore(t *testing.T) *idempotency.DynamoStore {
	t.Helper()
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYNAMODB_ENDPOINT not set")
	}
	db := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	table := fmt.Sprintf("idem%d", time.Now().UnixNano())
	_, err := db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName: aws.String("it_" + table),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return idempotency.NewDynamoStore(dynamo.NewBase(db, "it", table))
}

func TestDynamoStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s := localStore(t)

	if rec, err := s.Claim(ctx, "t1", "k", "h1", time.Minute); rec != nil || err != nil {
		t.Fatalf("first claim: %v %v", rec, err)
	}
	if _, err := s.Claim(ctx, "t1", "k", "h1", time.Minute); !errors.Is(err, idempotency.ErrInProgress) {
		t.Fatalf("same hash while running: %v", err)
	}
	if _, err := s.Claim(ctx, "t1", "k", "h2", time.Minute); !errors.Is(err, idempotency.ErrConflict) {
		t.Fatalf("other hash: %v", err)
	}
	if rec, err := s.Claim(ctx, "t2", "k", "h2", time.Minute); rec != nil || err != nil {
		t.Fatalf("other scope must be independent: %v %v", rec, err)
	}

	want := idempotency.Record{RequestHash: "h1", Status: 201, ContentType: "application/json", Body: []byte(`{"ok":true}`)}
	if err := s.Complete(ctx, "t1", "k", want, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claim(ctx, "t1", "k", "h1", time.Minute)
	if err != nil || got == nil || got.Status != 201 || string(got.Body) != `{"ok":true}` || got.ContentType != want.ContentType {
		t.Fatalf("replay: %+v %v", got, err)
	}
	// Release never removes an answer.
	if err := s.Release(ctx, "t1", "k"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Claim(ctx, "t1", "k", "h1", time.Minute); got == nil {
		t.Fatal("Release removed an answer")
	}
}

func TestDynamoStoreReleaseAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := localStore(t)
	if _, err := s.Claim(ctx, "t", "k", "h", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "t", "k"); err != nil {
		t.Fatal(err)
	}
	if rec, err := s.Claim(ctx, "t", "k", "h", -time.Second); rec != nil || err != nil {
		t.Fatalf("claim after release: %v %v", rec, err)
	}
	// The previous claim is already past its lease: a new one takes it over.
	if rec, err := s.Claim(ctx, "t", "k", "other", time.Minute); rec != nil || err != nil {
		t.Fatalf("lapsed claim not taken over: %v %v", rec, err)
	}
	if err := s.Release(ctx, "t", "missing"); err != nil {
		t.Fatalf("release of a missing key: %v", err)
	}
}

func TestDynamoStoreRaceHasOneWinner(t *testing.T) {
	s := localStore(t)
	var won atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec, err := s.Claim(context.Background(), "t", "race", "h", time.Minute); rec == nil && err == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d winners, want 1", won.Load())
	}
}
