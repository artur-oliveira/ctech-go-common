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
	"gopkg.aoctech.app/api-commons/ttlcache"
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
}

type Store struct {
	db        dynamoAPI
	table     string
	erasedTTL time.Duration
	now       func() time.Time
	// blocked remembers Blocked's answer per user for blockedMemoryTTL. It is read
	// on every write path of every request, so without it each request pays a
	// consistent DynamoDB read.
	blocked *ttlcache.Cache[bool]
}

// blockedMemoryTTL is how long a process serves a user's "blocked or not" from
// memory. A lock applied by this process (the consumer) is visible at once; one
// applied elsewhere is visible within this window.
const blockedMemoryTTL = 10 * time.Second

const blockedCacheMax = 20000

// WithBlockedMemoryTTL overrides how long Blocked's answer is remembered.
func (s *Store) WithBlockedMemoryTTL(d time.Duration) *Store {
	s.blocked = ttlcache.New[bool](d, blockedCacheMax)
	return s
}

// NewStore binds the service's erasure-state table. erasedTTL is how long a
// tombstone is kept (the service's longest retention); 0 keeps it forever.
func NewStore(db dynamoAPI, tablePrefix string, erasedTTL time.Duration) *Store {
	return &Store{db: db, table: dynamo.TableName(tablePrefix, TableSuffix), erasedTTL: erasedTTL, now: time.Now,
		blocked: ttlcache.New[bool](blockedMemoryTTL, blockedCacheMax)}
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
		// An active record newer than the erase means the user came back
		// (Clear) or the erase is from a request superseded by an unlock: stale.
		if cur != nil && prev.State == StateActive && seq <= prev.SeqNS {
			return prev, false
		}
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
	rec, _, err := s.ApplyChanged(ctx, key, m)
	return rec, err
}

// ApplyChanged is Apply that also reports whether this call moved the record
// to a different state (active, locked, erased). A no-op, a redelivery, a stale
// message, or a newer lock on an already locked record reports false.
func (s *Store) ApplyChanged(ctx context.Context, key string, m Message) (Record, bool, error) {
	for range 3 {
		cur, err := s.Get(ctx, key)
		if err != nil {
			return Record{}, false, err
		}
		next, changed := apply(cur, key, m, s.now(), s.erasedTTL)
		if !changed {
			return next, false, nil
		}
		err = s.put(ctx, next, cur)
		if dynamo.IsConditionFailed(err) {
			continue
		}
		if err != nil {
			return Record{}, false, fmt.Errorf("erasure: put %s: %w", key, err)
		}
		prevState := StateActive
		if cur != nil {
			prevState = cur.State
		}
		return next, next.State != prevState, nil
	}
	return Record{}, false, ErrConcurrentUpdate
}

func (s *Store) put(ctx context.Context, next Record, cur *Record) error {
	// Whatever the outcome, the remembered answer for this key is no longer trusted.
	defer s.blocked.Delete(next.PK)
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
	key := SubKey(sub)
	if b, ok := s.blocked.Get(key); ok {
		return b, nil
	}
	r, err := s.Get(ctx, key)
	if err != nil {
		// An error is not an answer; it is never remembered.
		return false, err
	}
	b := r != nil && r.State != StateActive
	s.blocked.Set(key, b)
	return b, nil
}

// OrgErased reports whether an organization was erased with its only member.
func (s *Store) OrgErased(ctx context.Context, orgID string) (bool, error) {
	r, err := s.Get(ctx, OrgKey(orgID))
	return r != nil && r.State == StateErased, err
}

// Clear reactivates sub when a user who left this service (scope service)
// consents to it again. The record is not deleted: it stays active with a
// sequence newer than the old erase, so a late duplicate of that erase is
// recognised as stale instead of wiping the returning user's new data.
// A record that is not erased is left alone.
func (s *Store) Clear(ctx context.Context, sub string) error {
	key := SubKey(sub)
	cur, err := s.Get(ctx, key)
	if err != nil || cur == nil || cur.State != StateErased {
		return err
	}
	now := s.now()
	next := Record{
		PK:        key,
		State:     StateActive,
		RequestID: cur.RequestID, // proves which erase was already done
		SeqNS:     max(cur.SeqNS, now.UnixNano()),
		UpdatedAt: now.UTC().Format(time.RFC3339),
		TTL:       now.Add(activeMemory).Unix(),
	}
	err = s.put(ctx, next, cur)
	if dynamo.IsConditionFailed(err) {
		return ErrConcurrentUpdate
	}
	return err
}

// Relink reactivates sub for a user.relinked message, ordered by its issued_at
// like lock/unlock: a relink older than the erasure it would reopen (a late
// duplicate delivered after the person left again) is ignored. Like Clear, the
// record is not deleted and a record that is not erased is left alone.
func (s *Store) Relink(ctx context.Context, sub string, issuedAt time.Time) error {
	key := SubKey(sub)
	cur, err := s.Get(ctx, key)
	if err != nil || cur == nil || cur.State != StateErased {
		return err
	}
	seq := issuedAt.UnixNano()
	if seq <= cur.SeqNS {
		return nil // stale: the erasure is newer than this relink
	}
	now := s.now()
	err = s.put(ctx, Record{
		PK:        key,
		State:     StateActive,
		RequestID: cur.RequestID, // proves which erase was already done
		SeqNS:     seq,
		UpdatedAt: now.UTC().Format(time.RFC3339),
		TTL:       now.Add(activeMemory).Unix(),
	}, cur)
	if dynamo.IsConditionFailed(err) {
		return ErrConcurrentUpdate
	}
	return err
}
