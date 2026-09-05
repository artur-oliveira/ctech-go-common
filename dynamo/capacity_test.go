package dynamo

import (
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func resetCapacityState(t *testing.T) {
	t.Helper()
	SetCapacityRecorder(nil)
	SetCapacitySampleRate(defaultCapacitySampleRate)
	t.Cleanup(func() {
		SetCapacityRecorder(nil)
		SetCapacitySampleRate(defaultCapacitySampleRate)
	})
}

func TestWantCapacity_NoRecorderNeverSamples(t *testing.T) {
	resetCapacityState(t)
	SetCapacitySampleRate(1)
	for range 100 {
		if wantCapacity() {
			t.Fatal("wantCapacity() = true with no recorder wired")
		}
	}
}

func TestWantCapacity_ZeroRateNeverSamples(t *testing.T) {
	resetCapacityState(t)
	SetCapacityRecorder(func(string, string, float64) {})
	SetCapacitySampleRate(0)
	for range 100 {
		if wantCapacity() {
			t.Fatal("wantCapacity() = true at sample rate 0")
		}
	}
}

func TestWantCapacity_FullRateAlwaysSamples(t *testing.T) {
	resetCapacityState(t)
	SetCapacityRecorder(func(string, string, float64) {})
	SetCapacitySampleRate(1)
	for range 100 {
		if !wantCapacity() {
			t.Fatal("wantCapacity() = false at sample rate 1")
		}
	}
}

func TestRecordConsumed_NilIsNoop(t *testing.T) {
	resetCapacityState(t)
	called := false
	SetCapacityRecorder(func(string, string, float64) { called = true })
	recordConsumed(OpGetItem, nil)
	if called {
		t.Fatal("recorder called for a nil ConsumedCapacity")
	}
}

func TestRecordConsumed_ReportsTableOperationAndUnits(t *testing.T) {
	resetCapacityState(t)
	var mu sync.Mutex
	var gotTable, gotOp string
	var gotUnits float64
	SetCapacityRecorder(func(table, operation string, units float64) {
		mu.Lock()
		defer mu.Unlock()
		gotTable, gotOp, gotUnits = table, operation, units
	})

	recordConsumed(OpQuery, &types.ConsumedCapacity{
		TableName:     aws.String("test_rooms"),
		CapacityUnits: aws.Float64(2.5),
	})

	mu.Lock()
	defer mu.Unlock()
	if gotTable != "test_rooms" || gotOp != OpQuery || gotUnits != 2.5 {
		t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", gotTable, gotOp, gotUnits, "test_rooms", OpQuery, 2.5)
	}
}

func TestRecordConsumed_NoCapacityUnitsIsNoop(t *testing.T) {
	resetCapacityState(t)
	called := false
	SetCapacityRecorder(func(string, string, float64) { called = true })
	recordConsumed(OpGetItem, &types.ConsumedCapacity{TableName: aws.String("t")})
	if called {
		t.Fatal("recorder called with no CapacityUnits set")
	}
}

func TestRecordConsumedMany_ReportsEveryEntry(t *testing.T) {
	resetCapacityState(t)
	var mu sync.Mutex
	var tables []string
	SetCapacityRecorder(func(table, _ string, _ float64) {
		mu.Lock()
		defer mu.Unlock()
		tables = append(tables, table)
	})

	recordConsumedMany(OpTransactWrite, []types.ConsumedCapacity{
		{TableName: aws.String("a"), CapacityUnits: aws.Float64(1)},
		{TableName: aws.String("b"), CapacityUnits: aws.Float64(3)},
	})

	mu.Lock()
	defer mu.Unlock()
	if len(tables) != 2 || tables[0] != "a" || tables[1] != "b" {
		t.Fatalf("got %v, want [a b]", tables)
	}
}
