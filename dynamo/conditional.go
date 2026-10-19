package dynamo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	// VersionAttr is the optimistic-concurrency counter UpdateVersioned reads
	// and bumps. An item without it is version 0.
	VersionAttr = "version"

	// DefaultMaxAttempts bounds both retry loops (transient conflicts in
	// ConditionalUpdate, lost version races in UpdateVersioned).
	DefaultMaxAttempts = 3

	// retryBackoff is the base delay between attempts (linear: n * base).
	retryBackoff = 20 * time.Millisecond

	expectedVersionToken = ":expected_version"
	transactionConflict  = "TransactionConflictException"
)

var (
	// ErrItemNotFound is returned by UpdateVersioned when the item is absent.
	ErrItemNotFound = errors.New("dynamo: item not found")
	// ErrVersionConflict is returned by UpdateVersioned when every attempt
	// lost the race to another writer. Nothing was written; map it to 409.
	ErrVersionConflict = errors.New("dynamo: version conflict, attempts exhausted")
)

func keyOf(pk string, sk *string) map[string]types.AttributeValue {
	key := map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: pk}}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}
	return key
}

// isTransient reports a write that never happened because it collided with a
// transaction on the same item. Safe to repeat. A single-item UpdateItem
// reports it as TransactionConflictException (not as a cancelled transaction).
func isTransient(err error) bool {
	if IsTransactionConflict(err) {
		return true
	}
	var ae interface{ ErrorCode() string }
	return errors.As(err, &ae) && ae.ErrorCode() == transactionConflict
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ConditionalUpdate applies updates (nil value = REMOVE) only if condition
// holds, in one UpdateItem:
//
//	(true, nil)   applied
//	(false, nil)  condition not met (a verdict about the caller's own
//	              condition: NEVER retried, the caller decides)
//	(false, err)  serialization or transport error
//
// Only a TransactionConflict (a concurrent transaction touched the item, so no
// condition was evaluated and nothing was written) is retried, up to
// DefaultMaxAttempts with backoff, honoring ctx. condNames/condValues are
// merged into the expression maps; a token clashing with one the updates
// produced (#attr / :attr) with a different meaning is an error.
func (b *Base) ConditionalUpdate(
	ctx context.Context,
	pk string, sk *string,
	updates map[string]any,
	condition string,
	condNames map[string]string,
	condValues map[string]types.AttributeValue,
) (bool, error) {
	expr, names, values, err := buildUpdateExpr(updates)
	if err != nil {
		return false, err
	}
	for k, v := range condNames {
		if old, dup := names[k]; dup && old != v {
			return false, fmt.Errorf("dynamo: condition name %s clashes with an update attribute", k)
		}
		names[k] = v
	}
	for k, v := range condValues {
		if _, dup := values[k]; dup {
			return false, fmt.Errorf("dynamo: condition value %s clashes with an update value", k)
		}
		values[k] = v
	}
	input := &dynamodb.UpdateItemInput{
		TableName:                aws.String(b.TableName),
		Key:                      keyOf(pk, sk),
		UpdateExpression:         aws.String(expr),
		ConditionExpression:      aws.String(condition),
		ExpressionAttributeNames: names,
	}
	if len(values) > 0 {
		input.ExpressionAttributeValues = values
	}

	for attempt := 1; ; attempt++ {
		_, err := b.db.UpdateItem(ctx, input)
		if err == nil {
			return true, nil
		}
		if IsConditionFailed(err) {
			return false, nil
		}
		if !isTransient(err) || attempt >= DefaultMaxAttempts {
			return false, wrapDynamoErr(err)
		}
		if err := sleepCtx(ctx, time.Duration(attempt)*retryBackoff); err != nil {
			return false, err
		}
	}
}

// UpdateVersioned is an optimistic read-modify-write: read the item, let
// mutate compute the attributes to change from that fresh copy, and write them
// guarded by "version is still what I read" (VersionAttr, bumped by 1; absent
// counts as 0, and the item must still exist).
//
// Lost race (someone else bumped the version) re-reads and runs mutate again,
// up to maxAttempts (<= 0 means DefaultMaxAttempts), then ErrVersionConflict.
// That retry is safe because mutate sees the new state: business preconditions
// ("status must be pending") belong inside mutate, and an error returned by it
// is returned as-is and never retried. For a fixed caller-owned condition use
// ConditionalUpdate instead. mutate returning no updates is a no-op (nil).
// Absent item: ErrItemNotFound. mutate may run several times: keep it free of
// side effects.
func (b *Base) UpdateVersioned(
	ctx context.Context,
	pk string, sk *string,
	mutate func(item map[string]types.AttributeValue) (map[string]any, error),
	maxAttempts int,
) error {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	var skArg []string
	if sk != nil {
		skArg = []string{*sk}
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		item, err := b.GetItem(ctx, pk, skArg...)
		if err != nil {
			return err
		}
		if item == nil {
			return ErrItemNotFound
		}
		var version int64
		av, hasVersion := item[VersionAttr]
		if hasVersion {
			if err := attributevalue.Unmarshal(av, &version); err != nil {
				return fmt.Errorf("dynamo: %s is not a number: %w", VersionAttr, err)
			}
		}
		updates, err := mutate(item)
		if err != nil {
			return err
		}
		if len(updates) == 0 {
			return nil
		}
		updates = maps.Clone(updates)
		updates[VersionAttr] = version + 1

		condition := "attribute_exists(pk) AND attribute_not_exists(#" + VersionAttr + ")"
		var condValues map[string]types.AttributeValue
		if hasVersion {
			condition = "#" + VersionAttr + " = " + expectedVersionToken
			condValues = map[string]types.AttributeValue{
				expectedVersionToken: &types.AttributeValueMemberN{Value: strconv.FormatInt(version, 10)},
			}
		}
		applied, err := b.ConditionalUpdate(ctx, pk, sk, updates, condition,
			map[string]string{"#" + VersionAttr: VersionAttr}, condValues)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		if attempt < maxAttempts {
			if err := sleepCtx(ctx, time.Duration(attempt)*retryBackoff); err != nil {
				return err
			}
		}
	}
	return ErrVersionConflict
}
