package erasure

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/dynamo"
)

// TableSuffix names each participant's state table, {prefix}_erasure_state:
// partition key "pk" (S) only, TTL attribute "ttl". Each service's infra creates it.
const TableSuffix = "erasure_state"

// activeMemory keeps an unlocked record long enough that a lock delivered late
// (older issued_at) is still recognised as stale.
const activeMemory = 30 * 24 * time.Hour

type State string

const (
	StateActive State = "active"
	StateLocked State = "locked" // refuse state-changing operations
	StateErased State = "erased" // terminal tombstone: drop any work for this key
)

type Record struct {
	PK        string `dynamodbav:"pk"`
	State     State  `dynamodbav:"erasure_state"`
	RequestID string `dynamodbav:"request_id"`
	SeqNS     int64  `dynamodbav:"seq_ns"` // issued_at (ns) of the newest message applied
	UpdatedAt string `dynamodbav:"updated_at"`
	TTL       int64  `dynamodbav:"ttl,omitempty"`
}

func SubKey(sub string) string   { return "SUB#" + sub }
func OrgKey(orgID string) string { return "ORG#" + orgID }

var ErrConcurrentUpdate = errors.New("erasure: state changed concurrently, retries exhausted")

type dynamoAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

type Store struct {
	db        dynamoAPI
	table     string
	erasedTTL time.Duration
	now       func() time.Time
}

// NewStore binds the service's erasure-state table. erasedTTL is how long a
// tombstone is kept (the service's longest retention); 0 keeps it forever.
func NewStore(db dynamoAPI, tablePrefix string, erasedTTL time.Duration) *Store {
	return &Store{db: db, table: dynamo.TableName(tablePrefix, TableSuffix), erasedTTL: erasedTTL, now: time.Now}
}

// apply is the whole state machine. Lock/unlock are ordered by issued_at
// because SQS is not; erase always wins and is terminal.
func apply(cur *Record, key string, m Message, now time.Time, erasedTTL time.Duration) (Record, bool) {
	prev := Record{PK: key, State: StateActive}
	if cur != nil {
		prev = *cur
	}
	if prev.State == StateErased {
		return prev, false
	}
	seq := m.IssuedAt.UnixNano()
	next := Record{PK: key, RequestID: m.RequestID, SeqNS: max(prev.SeqNS, seq), UpdatedAt: now.UTC().Format(time.RFC3339)}
	switch m.Type {
	case TypeErase:
		next.State = StateErased
		if erasedTTL > 0 {
			next.TTL = now.Add(erasedTTL).Unix()
		}
		return next, true
	case TypeLocked, TypeUnlocked:
		if seq <= prev.SeqNS {
			return prev, false
		}
		next.State = StateLocked
		if m.Type == TypeUnlocked {
			next.State = StateActive
			next.TTL = now.Add(activeMemory).Unix()
		}
		return next, true
	}
	return prev, false
}

func (s *Store) Get(ctx context.Context, key string) (*Record, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: key}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("erasure: get %s: %w", key, err)
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	var r Record
	if err := attributevalue.UnmarshalMap(out.Item, &r); err != nil {
		return nil, fmt.Errorf("erasure: decode %s: %w", key, err)
	}
	return &r, nil
}

// Apply moves the record at key according to m. Idempotent: redelivering the
// same message is a no-op. Uses optimistic concurrency against other consumers.
func (s *Store) Apply(ctx context.Context, key string, m Message) (Record, error) {
	for range 3 {
		cur, err := s.Get(ctx, key)
		if err != nil {
			return Record{}, err
		}
		next, changed := apply(cur, key, m, s.now(), s.erasedTTL)
		if !changed {
			return next, nil
		}
		err = s.put(ctx, next, cur)
		if dynamo.IsConditionFailed(err) {
			continue
		}
		if err != nil {
			return Record{}, fmt.Errorf("erasure: put %s: %w", key, err)
		}
		return next, nil
	}
	return Record{}, ErrConcurrentUpdate
}

func (s *Store) put(ctx context.Context, next Record, cur *Record) error {
	item, err := attributevalue.MarshalMap(next)
	if err != nil {
		return err
	}
	in := &dynamodb.PutItemInput{TableName: aws.String(s.table), Item: item}
	if cur == nil {
		in.ConditionExpression = aws.String("attribute_not_exists(pk)")
	} else {
		in.ConditionExpression = aws.String("seq_ns = :old_seq AND erasure_state = :old_state")
		in.ExpressionAttributeValues = map[string]types.AttributeValue{
			":old_seq":   &types.AttributeValueMemberN{Value: strconv.FormatInt(cur.SeqNS, 10)},
			":old_state": &types.AttributeValueMemberS{Value: string(cur.State)},
		}
	}
	_, err = s.db.PutItem(ctx, in)
	return err
}

// Blocked reports whether sub must be refused state-changing operations
// (locked or erased). Services call it on every write path for a user.
func (s *Store) Blocked(ctx context.Context, sub string) (bool, error) {
	r, err := s.Get(ctx, SubKey(sub))
	return r != nil && r.State != StateActive, err
}

// OrgErased reports whether an organization was erased with its only member.
func (s *Store) OrgErased(ctx context.Context, orgID string) (bool, error) {
	r, err := s.Get(ctx, OrgKey(orgID))
	return r != nil && r.State == StateErased, err
}

// Clear removes sub's tombstone when a user who left this service (scope
// service) consents to it again. A record that is not erased is left alone.
func (s *Store) Clear(ctx context.Context, sub string) error {
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 aws.String(s.table),
		Key:                       map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: SubKey(sub)}},
		ConditionExpression:       aws.String("erasure_state = :erased"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":erased": &types.AttributeValueMemberS{Value: string(StateErased)}},
	})
	if dynamo.IsConditionFailed(err) {
		return nil
	}
	return err
}
