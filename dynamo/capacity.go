package dynamo

import (
	"math/rand/v2"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Operation names passed to CapacityRecorder. A bounded set, same rule as any
// other metric dimension — never a table id, an item key, or a request id.
const (
	OpGetItem       = "GetItem"
	OpQuery         = "Query"
	OpBatchGetItem  = "BatchGetItem"
	OpUpdateItem    = "UpdateItem"
	OpTransactWrite = "TransactWriteItems"
)

// defaultCapacitySampleRate keeps ReturnConsumedCapacity off most calls: the
// field adds response cost, and every caller of this package pays it fleet-
// wide the moment a recorder is wired in.
const defaultCapacitySampleRate = 0.05

// CapacityRecorder receives one sampled consumed-capacity observation for a
// completed DynamoDB call. table is the physical (already-prefixed) table
// name; operation is one of the Op* constants above.
//
// This package cannot itself emit a metric — it must not depend on any one
// service's metrics sink — so a caller wires its own via SetCapacityRecorder
// at startup. Never call it synchronously with anything that itself blocks:
// it runs on the calling goroutine, right after the DynamoDB round trip.
type CapacityRecorder func(table, operation string, capacityUnits float64)

var (
	capacityMu       sync.RWMutex
	capacityRecorder CapacityRecorder
	capacitySampleRate = defaultCapacitySampleRate
)

// SetCapacityRecorder wires the sink for sampled ReturnConsumedCapacity
// observations across Query, GetItem, BatchGetItem, UpdateItem and
// TransactWrite. Call once at startup. A nil recorder (the default) means
// ReturnConsumedCapacity is never requested, so a caller that never wires one
// pays nothing extra.
func SetCapacityRecorder(fn CapacityRecorder) {
	capacityMu.Lock()
	defer capacityMu.Unlock()
	capacityRecorder = fn
}

// SetCapacitySampleRate overrides the fraction (0..1) of calls that request
// ReturnConsumedCapacity. Defaults to defaultCapacitySampleRate.
func SetCapacitySampleRate(rate float64) {
	capacityMu.Lock()
	defer capacityMu.Unlock()
	capacitySampleRate = rate
}

// wantCapacity reports whether the next call should request
// ReturnConsumedCapacity, and returns the recorder to hand the result to (nil
// if none is wired, in which case the caller must not set the field at all).
func wantCapacity() bool {
	capacityMu.RLock()
	recorder := capacityRecorder
	rate := capacitySampleRate
	capacityMu.RUnlock()
	if recorder == nil || rate <= 0 {
		return false
	}
	return rand.Float64() < rate
}

// recordConsumed reports a single *types.ConsumedCapacity (GetItem, Query,
// UpdateItem). No-ops if cc is nil (ReturnConsumedCapacity was not requested,
// or this call was not sampled).
func recordConsumed(operation string, cc *types.ConsumedCapacity) {
	if cc == nil {
		return
	}
	recordConsumedValue(operation, cc)
}

// recordConsumedMany reports every element of a []types.ConsumedCapacity
// (BatchGetItem, TransactWriteItems can touch more than one table).
func recordConsumedMany(operation string, ccs []types.ConsumedCapacity) {
	for i := range ccs {
		recordConsumedValue(operation, &ccs[i])
	}
}

func recordConsumedValue(operation string, cc *types.ConsumedCapacity) {
	capacityMu.RLock()
	recorder := capacityRecorder
	capacityMu.RUnlock()
	if recorder == nil || cc.CapacityUnits == nil {
		return
	}
	table := ""
	if cc.TableName != nil {
		table = *cc.TableName
	}
	recorder(table, operation, *cc.CapacityUnits)
}
