//go:build integration

// Integration tests for Base.Query against DynamoDB Local.
//
//	docker compose -f docker-compose.test.yml up -d
//	DYNAMODB_ENDPOINT=http://localhost:8131 go test -tags integration -race -count=1 ./dynamo/
//
// They exist because the bug they cover cannot be reproduced against a fake:
// DynamoDB applies Limit to the items it *evaluates*, before the
// FilterExpression, so a filtered query with a Limit can return an empty or
// short page while matching items sit further down the partition.
package dynamo

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func localClient(t *testing.T) *dynamodb.Client {
	t.Helper()
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYNAMODB_ENDPOINT not set")
	}
	return dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
}

// newQueryTable creates a fresh pk/sk table with a "by-owner" GSI
// (owner_pk/created_at) and returns a Base over it. The table is dropped when the
// test ends.
func newQueryTable(t *testing.T) (*Base, *dynamodb.Client) {
	t.Helper()
	db := localClient(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("it%d", time.Now().UnixNano())
	b := NewBase(db, prefix, "query")
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(b.TableName),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("owner_pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("created_at"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName: aws.String("by-owner"),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("owner_pk"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("created_at"), KeyType: types.KeyTypeRange},
			},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(b.TableName)})
	})
	return &b, db
}

// seed writes n rows into partition pk, sk = ROW#000..ROW#n-1, with
// status "match" where match(i) holds and "other" elsewhere. Every row is
// also in the GSI partition owner_pk=pk with created_at mirroring sk.
func seed(t *testing.T, b *Base, pk string, n int, match func(i int) bool) []string {
	t.Helper()
	var want []string
	for i := range n {
		status := "other"
		if match(i) {
			status = "match"
		}
		sk := fmt.Sprintf("ROW#%03d", i)
		if status == "match" {
			want = append(want, sk)
		}
		err := b.PutItem(context.Background(), map[string]types.AttributeValue{
			"pk":         &types.AttributeValueMemberS{Value: pk},
			"sk":         &types.AttributeValueMemberS{Value: sk},
			"owner_pk":   &types.AttributeValueMemberS{Value: pk},
			"created_at": &types.AttributeValueMemberS{Value: sk},
			"status":     &types.AttributeValueMemberS{Value: status},
			"tags":       &types.AttributeValueMemberL{Value: []types.AttributeValue{&types.AttributeValueMemberS{Value: status}}},
			"payload":    &types.AttributeValueMemberS{Value: "p-" + sk},
		})
		if err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	return want
}

func sks(t *testing.T, items []map[string]types.AttributeValue) []string {
	t.Helper()
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it["sk"].(*types.AttributeValueMemberS)
		if !ok {
			t.Fatalf("item without sk: %v", it)
		}
		out = append(out, s.Value)
	}
	return out
}

// paginate follows the cursor until it is nil, checking every page.
func paginate(t *testing.T, b *Base, opts QueryOpts) []string {
	t.Helper()
	var got []string
	for range 1000 {
		res, err := b.Query(context.Background(), opts)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(res.Items) > opts.Limit {
			t.Fatalf("page of %d items exceeds limit %d", len(res.Items), opts.Limit)
		}
		got = append(got, sks(t, res.Items)...)
		if res.LastEvaluatedKey == nil {
			return got
		}
		opts.ExclusiveStartKey = res.LastEvaluatedKey
	}
	t.Fatal("pagination did not terminate")
	return nil
}

// The reproduction: the only matching rows are past the first evaluated page.
// A single DynamoDB call with Limit=3 evaluates ROW#000..ROW#002, filters them
// all out and returns an empty page.
func TestFilteredQueryFindsMatchesBeyondTheFirstPage(t *testing.T) {
	b, _ := newQueryTable(t)
	want := seed(t, b, "P", 30, func(i int) bool { return i >= 25 })

	res, err := b.Query(context.Background(), QueryOpts{
		PK: "P", ScanIndexForward: true, Limit: 3,
		FilterField: "status", FilterValue: "match",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sks(t, res.Items); !slices.Equal(got, want[:3]) {
		t.Fatalf("got %v, want %v", got, want[:3])
	}
	if res.LastEvaluatedKey == nil {
		t.Fatal("two matches remain, cursor must not be nil")
	}
	rest, err := b.Query(context.Background(), QueryOpts{
		PK: "P", ScanIndexForward: true, Limit: 3,
		FilterField: "status", FilterValue: "match", ExclusiveStartKey: res.LastEvaluatedKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sks(t, rest.Items); !slices.Equal(got, want[3:]) {
		t.Fatalf("second page got %v, want %v", got, want[3:])
	}
	if rest.LastEvaluatedKey != nil {
		t.Fatalf("partition exhausted, cursor must be nil, got %v", rest.LastEvaluatedKey)
	}
}

// The contains() filter goes through the same path.
func TestContainsFilteredQueryFindsMatchesBeyondTheFirstPage(t *testing.T) {
	b, _ := newQueryTable(t)
	want := seed(t, b, "P", 20, func(i int) bool { return i == 17 })

	res, err := b.Query(context.Background(), QueryOpts{
		PK: "P", ScanIndexForward: true, Limit: 5,
		FilterContainsField: "tags", FilterContainsValue: "match",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sks(t, res.Items); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Following the cursor must visit every match exactly once, in order, for
// every page size, both directions — including pages where the last DynamoDB
// call evaluated more matches than the caller asked for (the cursor is then
// the key of the last returned item, not the raw LastEvaluatedKey).
func TestFilteredQueryPaginationIsExact(t *testing.T) {
	b, _ := newQueryTable(t)
	pattern := func(i int) bool { return i%7 == 0 || i%7 == 1 || i%5 == 3 || (i > 40 && i < 50) }
	want := seed(t, b, "P", 80, pattern)
	reversed := slices.Clone(want)
	slices.Reverse(reversed)

	for limit := 1; limit <= 9; limit++ {
		for _, forward := range []bool{true, false} {
			t.Run(fmt.Sprintf("limit=%d/forward=%v", limit, forward), func(t *testing.T) {
				got := paginate(t, b, QueryOpts{
					PK: "P", ScanIndexForward: forward, Limit: limit,
					FilterField: "status", FilterValue: "match",
				})
				exp := want
				if !forward {
					exp = reversed
				}
				if !slices.Equal(got, exp) {
					t.Fatalf("got %v\nwant %v", got, exp)
				}
			})
		}
	}
}

// Every page but the last is full: a short page with a cursor only happens
// when the safety cap is hit, which this partition is too small to reach.
func TestFilteredQueryPagesAreFull(t *testing.T) {
	b, _ := newQueryTable(t)
	want := seed(t, b, "P", 60, func(i int) bool { return i%4 == 1 })
	opts := QueryOpts{PK: "P", ScanIndexForward: true, Limit: 4, FilterField: "status", FilterValue: "match"}
	var got []string
	for {
		res, err := b.Query(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, sks(t, res.Items)...)
		if res.LastEvaluatedKey == nil {
			break
		}
		if len(res.Items) != 4 {
			t.Fatalf("short page (%d) with a cursor", len(res.Items))
		}
		opts.ExclusiveStartKey = res.LastEvaluatedKey
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// On a GSI the cursor needs the index keys as well as the table keys.
func TestFilteredGSIQueryPaginationIsExact(t *testing.T) {
	b, _ := newQueryTable(t)
	want := seed(t, b, "O", 50, func(i int) bool { return i%3 != 0 })
	_ = seed(t, b, "X", 10, func(int) bool { return true })

	for limit := 1; limit <= 6; limit++ {
		got := paginate(t, b, QueryOpts{
			IndexName: "by-owner", PKField: "owner_pk", SKField: "created_at",
			PK: "O", ScanIndexForward: true, Limit: limit,
			FilterField: "status", FilterValue: "match",
		})
		if !slices.Equal(got, want) {
			t.Fatalf("limit=%d got %v\nwant %v", limit, got, want)
		}
	}
}

// A projection that leaves out the key attributes still paginates exactly,
// and the returned items carry only what the caller projected.
func TestFilteredQueryWithProjectionPaginatesExactly(t *testing.T) {
	b, _ := newQueryTable(t)
	want := seed(t, b, "P", 40, func(i int) bool { return i%2 == 0 || i > 30 })

	opts := QueryOpts{
		PK: "P", ScanIndexForward: true, Limit: 3,
		FilterField: "status", FilterValue: "match",
		ProjectionExpression: "payload",
	}
	var got []string
	for range 1000 {
		res, err := b.Query(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range res.Items {
			if len(it) != 1 {
				t.Fatalf("projection leaked attributes: %v", it)
			}
			got = append(got, it["payload"].(*types.AttributeValueMemberS).Value[2:])
		}
		if res.LastEvaluatedKey == nil {
			break
		}
		opts.ExclusiveStartKey = res.LastEvaluatedKey
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

// A sparse filter over a large partition stops at the page cap and hands back
// a short (here empty) page with a cursor rather than sweeping the partition;
// following the cursor still reaches the match.
func TestFilteredQueryStopsAtThePageCap(t *testing.T) {
	b, _ := newQueryTable(t)
	want := seed(t, b, "P", 60, func(i int) bool { return i == 55 })

	opts := QueryOpts{
		PK: "P", ScanIndexForward: true, Limit: 2, MaxPages: 3,
		FilterField: "status", FilterValue: "match",
	}
	res, err := b.Query(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.LastEvaluatedKey == nil {
		t.Fatalf("want an empty page with a cursor at the cap, got %d items, cursor %v", len(res.Items), res.LastEvaluatedKey)
	}
	if got := paginate(t, b, opts); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Without a filter the query is a single DynamoDB call, unchanged: Limit rows
// and the raw LastEvaluatedKey.
func TestUnfilteredQueryIsUnchanged(t *testing.T) {
	b, db := newQueryTable(t)
	seed(t, b, "P", 10, func(int) bool { return false })

	res, err := b.Query(context.Background(), QueryOpts{PK: "P", ScanIndexForward: true, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.Query(context.Background(), &dynamodb.QueryInput{
		TableName:                 aws.String(b.TableName),
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: "P"}},
		ScanIndexForward:          aws.Bool(true),
		Limit:                     aws.Int32(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sks(t, res.Items), sks(t, raw.Items)) {
		t.Fatalf("items differ: %v vs %v", sks(t, res.Items), sks(t, raw.Items))
	}
	if got, want := res.LastEvaluatedKey["sk"].(*types.AttributeValueMemberS).Value,
		raw.LastEvaluatedKey["sk"].(*types.AttributeValueMemberS).Value; got != want {
		t.Fatalf("cursor %q, want raw %q", got, want)
	}
}

// QueryRawFiltered gives a hand-built filtered query (here a numeric filter
// the typed QueryOpts cannot express) the same fill-and-cursor behaviour.
func TestQueryRawFilteredFindsMatchesAndPaginatesExactly(t *testing.T) {
	b, _ := newQueryTable(t)
	var want []string
	for i := range 40 {
		sk := fmt.Sprintf("ROW#%03d", i)
		year := "2025"
		if i >= 30 || i%9 == 4 {
			year = "2026"
			want = append(want, sk)
		}
		if err := b.PutItem(context.Background(), map[string]types.AttributeValue{
			"pk":   &types.AttributeValueMemberS{Value: "P"},
			"sk":   &types.AttributeValueMemberS{Value: sk},
			"year": &types.AttributeValueMemberN{Value: year},
		}); err != nil {
			t.Fatal(err)
		}
	}
	input := func(limit int32, start map[string]types.AttributeValue) *dynamodb.QueryInput {
		return &dynamodb.QueryInput{
			KeyConditionExpression:   aws.String("pk = :pk"),
			FilterExpression:         aws.String("#y = :y"),
			ExpressionAttributeNames: map[string]string{"#y": "year"},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: "P"},
				":y":  &types.AttributeValueMemberN{Value: "2026"},
			},
			Limit: aws.Int32(limit), ExclusiveStartKey: start,
		}
	}
	for limit := int32(1); limit <= 7; limit++ {
		var got []string
		var start map[string]types.AttributeValue
		for range 1000 {
			res, err := b.QueryRawFiltered(context.Background(), input(limit, start), 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Items) > int(limit) {
				t.Fatalf("page of %d exceeds limit %d", len(res.Items), limit)
			}
			if res.LastEvaluatedKey != nil && len(res.Items) != int(limit) {
				t.Fatalf("limit=%d: short page (%d) with a cursor", limit, len(res.Items))
			}
			got = append(got, sks(t, res.Items)...)
			if res.LastEvaluatedKey == nil {
				break
			}
			start = res.LastEvaluatedKey
		}
		if !slices.Equal(got, want) {
			t.Fatalf("limit=%d got %v\nwant %v", limit, got, want)
		}
	}
}

// Select COUNT is passed through as a single call.
func TestQueryRawFilteredCountIsASingleCall(t *testing.T) {
	b, _ := newQueryTable(t)
	seed(t, b, "P", 10, func(i int) bool { return i >= 5 })
	res, err := b.QueryRawFiltered(context.Background(), &dynamodb.QueryInput{
		KeyConditionExpression:   aws.String("pk = :pk"),
		FilterExpression:         aws.String("#s = :s"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: "P"},
			":s":  &types.AttributeValueMemberS{Value: "match"},
		},
		Limit: aws.Int32(3), Select: types.SelectCount,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.LastEvaluatedKey == nil {
		t.Fatalf("want the raw single COUNT call (no items, a cursor), got %d items, cursor %v", len(res.Items), res.LastEvaluatedKey)
	}
}
