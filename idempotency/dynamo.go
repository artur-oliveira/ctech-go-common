package idempotency

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/dynamo"
)

// Table layout (see README.md): pk = "IDEM#<scope>", sk = "KEY#<key>", TTL
// attribute "ttl" (epoch seconds; enable DynamoDB TTL on it).
const (
	PKPrefix = "IDEM#"
	SKPrefix = "KEY#"

	attrPK      = "pk"
	attrSK      = "sk"
	attrTTL     = "ttl"
	attrStatus  = "status"
	statusClaim = 0 // status of a claim that has no answer yet
)

// DynamoStore is the Store backed by a DynamoDB table.
type DynamoStore struct {
	base dynamo.Base
	now  func() time.Time
}

// NewDynamoStore wraps a dynamo.Base opened on the idempotency table.
func NewDynamoStore(base dynamo.Base) *DynamoStore {
	return &DynamoStore{base: base, now: time.Now}
}

type row struct {
	PK          string `dynamodbav:"pk"`
	SK          string `dynamodbav:"sk"`
	TTL         int64  `dynamodbav:"ttl"`
	RequestHash string `dynamodbav:"request_hash"`
	Status      int    `dynamodbav:"status"`
	ContentType string `dynamodbav:"content_type,omitempty"`
	Response    []byte `dynamodbav:"response,omitempty"`
	CreatedAt   string `dynamodbav:"created_at"`
}

func newRow(scope, key string, rec Record, expires, now time.Time) row {
	return row{
		PK: PKPrefix + scope, SK: SKPrefix + key, TTL: expires.Unix(),
		RequestHash: rec.RequestHash, Status: rec.Status,
		ContentType: rec.ContentType, Response: rec.Body,
		CreatedAt: now.UTC().Format(time.RFC3339Nano),
	}
}

func (s *DynamoStore) Claim(ctx context.Context, scope, key, requestHash string, lease time.Duration) (*Record, error) {
	now := s.now()
	item, err := dynamo.Encode(newRow(scope, key, Record{RequestHash: requestHash}, now.Add(lease), now))
	if err != nil {
		return nil, err
	}
	// Free, or expired: DynamoDB TTL deletion is lazy (up to ~48h), so the
	// condition itself must treat a lapsed entry as free.
	_, err = s.base.PutItemRaw(ctx, &dynamodb.PutItemInput{
		Item:                     item,
		ConditionExpression:      aws.String("attribute_not_exists(" + attrPK + ") OR #ttl < :now"),
		ExpressionAttributeNames: map[string]string{"#ttl": attrTTL},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Unix(), 10)},
		},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err == nil {
		return nil, nil
	}
	ccf, ok := errors.AsType[*types.ConditionalCheckFailedException](err)
	if !ok {
		return nil, err
	}
	if ccf.Item == nil {
		// The holder released or expired between our write and its report.
		return nil, ErrInProgress
	}
	cur, err := dynamo.Decode[row](ccf.Item)
	if err != nil {
		return nil, err
	}
	switch {
	case cur.RequestHash != requestHash:
		return nil, fmt.Errorf("%w: %s", ErrConflict, key)
	case cur.Status == statusClaim:
		return nil, fmt.Errorf("%w: %s", ErrInProgress, key)
	}
	return &Record{RequestHash: cur.RequestHash, Status: cur.Status, ContentType: cur.ContentType, Body: cur.Response}, nil
}

func (s *DynamoStore) Complete(ctx context.Context, scope, key string, rec Record, ttl time.Duration) error {
	now := s.now()
	item, err := dynamo.Encode(newRow(scope, key, rec, now.Add(ttl), now))
	if err != nil {
		return err
	}
	return s.base.PutItem(ctx, item)
}

func (s *DynamoStore) Release(ctx context.Context, scope, key string) error {
	_, err := s.base.DeleteItemRaw(ctx, &dynamodb.DeleteItemInput{
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: PKPrefix + scope},
			attrSK: &types.AttributeValueMemberS{Value: SKPrefix + key},
		},
		ConditionExpression:       aws.String("#s = :claim"),
		ExpressionAttributeNames:  map[string]string{"#s": attrStatus},
		ExpressionAttributeValues: map[string]types.AttributeValue{":claim": &types.AttributeValueMemberN{Value: strconv.Itoa(statusClaim)}},
	})
	if dynamo.IsConditionFailed(err) {
		return nil // an answer (or nothing) is there; never remove it
	}
	return err
}
