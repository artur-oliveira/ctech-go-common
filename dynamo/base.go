// Package dynamo provides the shared DynamoDB persistence primitives used
// across CTech services.
//
// Key design rules:
//   - get_item > query > scan  (no scans in production)
//   - transact_write for atomic multi-item operations
//   - Table names are prefixed by environment: {prefix}_{table}
package dynamo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Base provides common DynamoDB operations for all repositories.
type Base struct {
	db          *dynamodb.Client
	TableName   string
	tablePrefix string
}

// TableName returns the environment-prefixed physical table name
// ({prefix}_{table}). Exported for call sites outside the repository layer
// that need the physical name without a repository (e.g. a health probe).
func TableName(tablePrefix, table string) string {
	return fmt.Sprintf("%s_%s", tablePrefix, table)
}

// NewBase creates a Base repository with an environment-prefixed table name.
func NewBase(db *dynamodb.Client, tablePrefix, table string) Base {
	return Base{
		db:          db,
		TableName:   TableName(tablePrefix, table),
		tablePrefix: tablePrefix,
	}
}

// NowStr returns the current UTC time as ISO 8601.
func NowStr() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// GetItem fetches a single item by PK (and optional SK).
func (b *Base) GetItem(ctx context.Context, pk string, sk ...string) (map[string]types.AttributeValue, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if len(sk) > 0 && sk[0] != "" {
		key["sk"] = &types.AttributeValueMemberS{Value: sk[0]}
	}

	out, err := b.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(b.TableName),
		Key:       key,
	})
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	if out.Item == nil {
		return nil, nil
	}
	return out.Item, nil
}

// GetItemByRawKey fetches a single item using a caller-supplied key map.
// Use when the sort key is not a standard string "sk" field (e.g. numeric NSU).
func (b *Base) GetItemByRawKey(ctx context.Context, key map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	out, err := b.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(b.TableName),
		Key:       key,
	})
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	if out.Item == nil {
		return nil, nil
	}
	return out.Item, nil
}

// PutItem writes an item to the table.
func (b *Base) PutItem(ctx context.Context, item map[string]types.AttributeValue) error {
	_, err := b.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(b.TableName),
		Item:      item,
	})
	return wrapDynamoErr(err)
}

// buildUpdateExpr builds a combined SET/REMOVE update expression. Nil values
// become REMOVE clauses (clearing the attribute without storing a NULL);
// non-nil values become SET clauses.
func buildUpdateExpr(updates map[string]any) (string, map[string]string, map[string]types.AttributeValue, error) {
	setParts := make([]string, 0, len(updates))
	removeParts := make([]string, 0)
	exprNames := make(map[string]string, len(updates))
	exprValues := make(map[string]types.AttributeValue)

	for attr, val := range updates {
		exprNames["#"+attr] = attr
		if val == nil {
			removeParts = append(removeParts, "#"+attr)
			continue
		}
		av, err := attributevalue.Marshal(val)
		if err != nil {
			return "", nil, nil, err
		}
		setParts = append(setParts, fmt.Sprintf("#%s = :%s", attr, attr))
		exprValues[":"+attr] = av
	}

	clauses := make([]string, 0, 2)
	if len(setParts) > 0 {
		clauses = append(clauses, "SET "+strings.Join(setParts, ", "))
	}
	if len(removeParts) > 0 {
		clauses = append(clauses, "REMOVE "+strings.Join(removeParts, ", "))
	}
	return strings.Join(clauses, " "), exprNames, exprValues, nil
}

// UpdateItem applies a SET/REMOVE expression to an existing item. Nil values
// in updates clear the attribute via REMOVE instead of writing a NULL.
// Returns false if the item does not exist (ConditionCheckFailed).
func (b *Base) UpdateItem(ctx context.Context, pk string, sk *string, updates map[string]any) (bool, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	expr, exprNames, exprValues, err := buildUpdateExpr(updates)
	if err != nil {
		return false, err
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String(expr),
		ExpressionAttributeNames: exprNames,
		ConditionExpression:      aws.String("attribute_exists(pk)"),
	}
	if len(exprValues) > 0 {
		input.ExpressionAttributeValues = exprValues
	}

	_, err = b.db.UpdateItem(ctx, input)
	if err != nil {
		if isConditionFailed(err) {
			return false, nil
		}
		return false, wrapDynamoErr(err)
	}
	return true, nil
}

// UpsertAttrs applies a PARTIAL update, creating the item when absent. It is
// UpdateItem without the attribute_exists guard.
//
// Use it when the row holds several independently written fields (e.g. a
// user row's separately-written consent documents): a whole-row PutItem would
// clobber fields this writer does not own, while UpdateItem would silently
// drop the very first write because no row exists yet.
func (b *Base) UpsertAttrs(ctx context.Context, pk string, sk *string, updates map[string]any) error {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	expr, exprNames, exprValues, err := buildUpdateExpr(updates)
	if err != nil {
		return err
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String(expr),
		ExpressionAttributeNames: exprNames,
	}
	if len(exprValues) > 0 {
		input.ExpressionAttributeValues = exprValues
	}

	_, err = b.db.UpdateItem(ctx, input)
	return wrapDynamoErr(err)
}

// DeleteItem removes an item. Returns false if not found.
func (b *Base) DeleteItem(ctx context.Context, pk string, sk ...string) (bool, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if len(sk) > 0 && sk[0] != "" {
		key["sk"] = &types.AttributeValueMemberS{Value: sk[0]}
	}

	_, err := b.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:           aws.String(b.TableName),
		Key:                 key,
		ConditionExpression: aws.String("attribute_exists(pk)"),
	})
	if err != nil {
		if isConditionFailed(err) {
			return false, nil
		}
		return false, wrapDynamoErr(err)
	}
	return true, nil
}

// BuildPutTxItem returns a TransactWriteItem equivalent to PutItem, for composing
// a multi-item transaction via TransactWrite instead of writing immediately.
func (b *Base) BuildPutTxItem(item map[string]types.AttributeValue) types.TransactWriteItem {
	return types.TransactWriteItem{
		Put: &types.Put{
			TableName: aws.String(b.TableName),
			Item:      item,
		},
	}
}

// BuildPutTxItemIfAbsent is like BuildPutTxItem but fails the transaction if
// an item with the same key already exists — used for create-only semantics
// (e.g. person dedup by CPF/CNPJ) instead of the default overwrite-on-put.
func (b *Base) BuildPutTxItemIfAbsent(item map[string]types.AttributeValue) types.TransactWriteItem {
	return types.TransactWriteItem{
		Put: &types.Put{
			TableName:           aws.String(b.TableName),
			Item:                item,
			ConditionExpression: aws.String("attribute_not_exists(pk)"),
		},
	}
}

// BuildUpdateTxItem returns a TransactWriteItem equivalent to UpdateItem, for
// composing a multi-item transaction via TransactWrite instead of writing
// immediately. Same SET/REMOVE semantics and attribute_exists(pk) condition as
// UpdateItem.
func (b *Base) BuildUpdateTxItem(pk string, sk *string, updates map[string]any) (types.TransactWriteItem, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	expr, exprNames, exprValues, err := buildUpdateExpr(updates)
	if err != nil {
		return types.TransactWriteItem{}, err
	}

	update := &types.Update{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String(expr),
		ExpressionAttributeNames: exprNames,
		ConditionExpression:      aws.String("attribute_exists(pk)"),
	}
	if len(exprValues) > 0 {
		update.ExpressionAttributeValues = exprValues
	}
	return types.TransactWriteItem{Update: update}, nil
}

// BuildRawUpdateTxItem returns a TransactWriteItem with a caller-supplied update
// and condition expression. Used for balance math that must be relative and
// conditional in one atomic step (e.g. "SET balance = balance - :amt" guarded by
// "balance >= :amt") — semantics buildUpdateExpr's absolute SET cannot express.
func (b *Base) BuildRawUpdateTxItem(pk string, sk *string, updateExpr, condExpr string, names map[string]string, values map[string]types.AttributeValue) types.TransactWriteItem {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}
	update := &types.Update{
		TableName:                 aws.String(b.TableName),
		Key:                       key,
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	}
	if condExpr != "" {
		update.ConditionExpression = aws.String(condExpr)
	}
	return types.TransactWriteItem{Update: update}
}

// BuildDeleteTxItem returns a TransactWriteItem equivalent to DeleteItem, for
// composing a multi-item transaction via TransactWrite instead of writing
// immediately.
func (b *Base) BuildDeleteTxItem(pk string, sk ...string) types.TransactWriteItem {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if len(sk) > 0 && sk[0] != "" {
		key["sk"] = &types.AttributeValueMemberS{Value: sk[0]}
	}
	return types.TransactWriteItem{
		Delete: &types.Delete{
			TableName:           aws.String(b.TableName),
			Key:                 key,
			ConditionExpression: aws.String("attribute_exists(pk)"),
		},
	}
}

// QueryResult holds paginated query results.
type QueryResult struct {
	Items            []map[string]types.AttributeValue
	LastEvaluatedKey map[string]types.AttributeValue
}

// Query runs a DynamoDB Query with optional SK prefix and pagination.
func (b *Base) Query(ctx context.Context, opts QueryOpts) (*QueryResult, error) {
	opts.defaults()
	input := &dynamodb.QueryInput{
		TableName:                 aws.String(b.TableName),
		KeyConditionExpression:    aws.String(fmt.Sprintf("%s = :pk", opts.PKField)),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: opts.PK}},
		ScanIndexForward:          aws.Bool(opts.ScanIndexForward),
		Limit:                     aws.Int32(int32(opts.Limit)),
	}

	if opts.IndexName != "" {
		input.IndexName = aws.String(opts.IndexName)
	}
	if opts.SKPrefix != "" {
		input.KeyConditionExpression = aws.String(
			fmt.Sprintf("%s = :pk AND begins_with(#sk, :sk_prefix)", opts.PKField),
		)
		if input.ExpressionAttributeNames == nil {
			input.ExpressionAttributeNames = make(map[string]string)
		}
		input.ExpressionAttributeNames["#sk"] = opts.SKField
		input.ExpressionAttributeValues[":sk_prefix"] = &types.AttributeValueMemberS{Value: opts.SKPrefix}
	}
	if opts.ExclusiveStartKey != nil {
		input.ExclusiveStartKey = opts.ExclusiveStartKey
	}
	if opts.FilterField != "" {
		input.FilterExpression = aws.String("#filter_field = :filter_value")
		if input.ExpressionAttributeNames == nil {
			input.ExpressionAttributeNames = make(map[string]string)
		}
		input.ExpressionAttributeNames["#filter_field"] = opts.FilterField
		input.ExpressionAttributeValues[":filter_value"] = &types.AttributeValueMemberS{Value: opts.FilterValue}
	}

	out, err := b.db.Query(ctx, input)
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	return &QueryResult{Items: out.Items, LastEvaluatedKey: out.LastEvaluatedKey}, nil
}

// QueryOpts configures a Query call.
type QueryOpts struct {
	PK                string
	PKField           string // default "pk"
	SKField           string // default "sk"
	SKPrefix          string
	IndexName         string
	ScanIndexForward  bool
	Limit             int
	ExclusiveStartKey map[string]types.AttributeValue
	// FilterField/FilterValue apply a post-key-condition equality filter
	// (DynamoDB FilterExpression) — e.g. narrowing a GSI query whose partition
	// key isn't the org, back down to one org's rows. Both must be set together.
	FilterField string
	FilterValue string
}

func (o *QueryOpts) defaults() {
	if o.PKField == "" {
		o.PKField = "pk"
	}
	if o.SKField == "" {
		o.SKField = "sk"
	}
	if o.Limit == 0 {
		o.Limit = 100
	}
}

// QueryGSI queries a GSI by a single key attribute (equality). Aliases the
// key attribute (#k) so reserved words like "status" are legal index key names.
func (b *Base) QueryGSI(ctx context.Context, indexName, keyName, keyValue string, limit int, startKey map[string]types.AttributeValue) (*QueryResult, error) {
	if limit <= 0 {
		limit = 1
	}
	input := &dynamodb.QueryInput{
		TableName:                 aws.String(b.TableName),
		IndexName:                 aws.String(indexName),
		KeyConditionExpression:    aws.String("#k = :v"),
		ExpressionAttributeNames:  map[string]string{"#k": keyName},
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": &types.AttributeValueMemberS{Value: keyValue}},
		Limit:                     aws.Int32(int32(limit)),
	}
	if startKey != nil {
		input.ExclusiveStartKey = startKey
	}
	out, err := b.db.Query(ctx, input)
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	return &QueryResult{Items: out.Items, LastEvaluatedKey: out.LastEvaluatedKey}, nil
}

// UpdateItemRaw runs an arbitrary UpdateItem expression.
func (b *Base) UpdateItemRaw(ctx context.Context, input *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
	input.TableName = aws.String(b.TableName)
	out, err := b.db.UpdateItem(ctx, input)
	return out, wrapDynamoErr(err)
}

// TransactWrite executes a DynamoDB transact write with the provided items.
func (b *Base) TransactWrite(ctx context.Context, items []types.TransactWriteItem) error {
	_, err := b.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: items,
	})
	return wrapDynamoErr(err)
}

// AtomicIncrement increments a numeric field and returns the new value.
func (b *Base) AtomicIncrement(ctx context.Context, pk string, sk *string, field string) (int64, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	out, err := b.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String("ADD #f :inc SET updated_at = :now"),
		ExpressionAttributeNames: map[string]string{"#f": field},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":inc": &types.AttributeValueMemberN{Value: "1"},
			":now": &types.AttributeValueMemberS{Value: NowStr()},
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		return 0, wrapDynamoErr(err)
	}

	if av, ok := out.Attributes[field]; ok {
		if nv, ok := av.(*types.AttributeValueMemberN); ok {
			n, _ := strconv.ParseInt(nv.Value, 10, 64)
			return n, nil
		}
	}
	return 0, nil
}

// Decode unmarshals DynamoDB attribute values into the target struct.
func Decode[T any](item map[string]types.AttributeValue) (*T, error) {
	var out T
	if err := attributevalue.UnmarshalMap(item, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Encode marshals a value into DynamoDB attribute values, omitting nulls.
func Encode(v any) (map[string]types.AttributeValue, error) {
	return MarshalMapOmitNull(v)
}

func wrapDynamoErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("dynamodb: %w", err)
}

func isConditionFailed(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*types.ConditionalCheckFailedException](err); ok {
		return true
	}
	return strings.Contains(err.Error(), "ConditionalCheckFailed")
}

func isTransactionCanceled(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*types.TransactionCanceledException](err); ok {
		return true
	}
	return strings.Contains(err.Error(), "TransactionCanceledException")
}

// IsConditionFailed reports whether err represents a DynamoDB conditional
// check failure, either from a single-item call or from within a
// TransactWrite (TransactionCanceledException wrapping a condition failure).
// Exported for the services layer to translate into problem.Conflict.
func IsConditionFailed(err error) bool {
	return isConditionFailed(err) || isTransactionCanceled(err)
}
