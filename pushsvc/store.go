package pushsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/dynamo"
)

// One table per product, `{prefix}_push`:
//
//	pk = USER#{sub}  sk = SUB#{sha256(endpoint)[:16]}  one browser subscription (expires after SubscriptionTTL)
//	                 sk = META#counts                  subscriptions count, the limit's guard
//	                 sk = REMIND#{kind}                a scheduled reminder (reminder.go)
//
// GSIs the table must define: IndexEndpoint (pk endpoint_id, sk pk, KEYS_ONLY) and IndexSlot (pk slot_key, sk kind_sub,
// KEYS_ONLY). The endpoint and keys are not sealed: a push service refuses a send without the VAPID private key and the
// table is encrypted at rest.
const (
	// TableName is the logical table name; the physical one is `{prefix}_push`.
	TableName = "push"
	// IndexEndpoint finds the people who hold one browser endpoint.
	IndexEndpoint = "endpoint-index"
	// IndexSlot finds the reminders due in one 15-minute slot.
	IndexSlot = "slot-index"
	// MaxSubscriptionsPerUser caps a person's devices; the 11th replaces the one seen least recently.
	MaxSubscriptionsPerUser = 10
	// SubscriptionTTL is how long a row lives without a sync renewing it.
	SubscriptionTTL = 60 * 24 * time.Hour

	userPrefix   = "USER#"
	subPrefix    = "SUB#"
	countsSK     = "META#counts"
	attrEndpoint = "endpoint_id"
)

// ErrNotFound reports a subscription that does not exist for this person.
var ErrNotFound = errors.New("pushsvc: subscription not found")

// Subscription is one browser of one person.
type Subscription struct {
	ID         string `dynamodbav:"id"           json:"id"`
	Endpoint   string `dynamodbav:"endpoint"     json:"-"`
	P256dh     string `dynamodbav:"p256dh"       json:"-"`
	Auth       string `dynamodbav:"auth"         json:"-"`
	VapidKID   string `dynamodbav:"vapid_kid"    json:"vapid_kid"`
	Device     string `dynamodbav:"device"       json:"device"`
	CreatedAt  string `dynamodbav:"created_at"   json:"created_at"`
	LastSeenAt string `dynamodbav:"last_seen_at" json:"last_seen_at"`
}

// String redacts: the endpoint is a per-subscription token and never reaches a log.
func (s Subscription) String() string { return "Subscription{id=" + s.ID + "}" }

// GoString redacts like String, for %#v.
func (s Subscription) GoString() string { return s.String() }

// SubscriptionID names a subscription by its endpoint: 16 hex characters of the SHA-256. An upsert of the same browser
// lands on the same row.
func SubscriptionID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:8])
}

func userPK(sub string) string { return userPrefix + sub }
func subSK(id string) string   { return subPrefix + id }

// stamp is fixed width (nanoseconds always printed), so comparing two stamps as strings orders them in time.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func nAV(n int) types.AttributeValue { return &types.AttributeValueMemberN{Value: strconv.Itoa(n)} }
func sAV(v string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: v}
}

// Store reads and writes subscriptions.
type Store struct {
	base dynamo.Base
	now  func() time.Time
}

// NewStore returns a Store over `{tablePrefix}_push`.
func NewStore(db *dynamodb.Client, tablePrefix string) *Store {
	return &Store{base: dynamo.NewBase(db, tablePrefix, TableName), now: time.Now}
}

// List returns the person's devices, oldest first by key. At most MaxSubscriptionsPerUser rows exist.
func (s *Store) List(ctx context.Context, sub string) ([]Subscription, error) {
	res, err := s.base.Query(ctx, dynamo.QueryOpts{
		PK: userPK(sub), SKPrefix: subPrefix, ScanIndexForward: true, Limit: MaxSubscriptionsPerUser + 1,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Subscription, 0, len(res.Items))
	for _, it := range res.Items {
		var d Subscription
		if err := attributevalue.UnmarshalMap(it, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Store) item(sub string, d Subscription, now time.Time) (map[string]types.AttributeValue, error) {
	item, err := dynamo.MarshalMapOmitNull(d)
	if err != nil {
		return nil, err
	}
	item["pk"] = sAV(userPK(sub))
	item["sk"] = sAV(subSK(d.ID))
	item[attrEndpoint] = sAV(d.ID)
	item["ttl"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(SubscriptionTTL).Unix(), 10)}
	return item, nil
}

// OtherOwners returns the people other than `sub` who hold a subscription for this endpoint. One browser endpoint
// belongs to one person at a time: when someone signs in on a shared browser the previous person's row must go, or both
// would be notified on the same screen. The index is eventually consistent; the logout cleanup is the primary guard and
// this corrects a row the logout never reached.
func (s *Store) OtherOwners(ctx context.Context, endpoint, sub string) ([]string, error) {
	id := SubscriptionID(endpoint)
	mine := userPK(sub)
	var owners []string
	var start map[string]types.AttributeValue
	for {
		res, err := s.base.Query(ctx, dynamo.QueryOpts{
			PK: id, PKField: attrEndpoint, IndexName: IndexEndpoint, Limit: 25, ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			pk, ok := it["pk"].(*types.AttributeValueMemberS)
			if ok && pk.Value != mine && strings.HasPrefix(pk.Value, userPrefix) {
				owners = append(owners, strings.TrimPrefix(pk.Value, userPrefix))
			}
		}
		if res.LastEvaluatedKey == nil {
			return owners, nil
		}
		start = res.LastEvaluatedKey
	}
}

// Upsert registers or renews a device and returns its id. A known endpoint only renews last_seen_at and the keys (a PUT
// is idempotent). A new one takes a slot under the count row's guard; a full person loses the device seen least
// recently, in the same transaction, so the count never passes MaxSubscriptionsPerUser.
func (s *Store) Upsert(ctx context.Context, sub string, d Subscription) (string, error) {
	now := s.now()
	d.ID = SubscriptionID(d.Endpoint)
	d.LastSeenAt = stamp(now)
	existing, err := s.List(ctx, sub)
	if err != nil {
		return "", err
	}
	if i := slices.IndexFunc(existing, func(e Subscription) bool { return e.ID == d.ID }); i >= 0 {
		d.CreatedAt = existing[i].CreatedAt
		item, err := s.item(sub, d, now)
		if err != nil {
			return "", err
		}
		return d.ID, s.base.PutItem(ctx, item)
	}
	d.CreatedAt = d.LastSeenAt
	item, err := s.item(sub, d, now)
	if err != nil {
		return "", err
	}
	pk := userPK(sub)
	counts := countsSK
	if len(existing) < MaxSubscriptionsPerUser {
		count := s.base.BuildRawUpdateTxItem(pk, &counts,
			"SET subscriptions = if_not_exists(subscriptions, :zero) + :one",
			"attribute_not_exists(subscriptions) OR subscriptions < :max", nil,
			map[string]types.AttributeValue{":zero": nAV(0), ":one": nAV(1), ":max": nAV(MaxSubscriptionsPerUser)})
		err = s.base.TransactWrite(ctx, []types.TransactWriteItem{count, s.base.BuildPutTxItemIfAbsent(item)})
		if err == nil || !dynamo.IsConditionFailed(err) {
			return d.ID, err
		}
		// Another request filled the last slot meanwhile: evict like a full person.
		if existing, err = s.List(ctx, sub); err != nil {
			return "", err
		}
	}
	if len(existing) == 0 {
		return d.ID, errors.New("pushsvc: subscription slots are full but no device was found")
	}
	oldest := existing[0]
	for _, e := range existing[1:] {
		if e.LastSeenAt < oldest.LastSeenAt {
			oldest = e
		}
	}
	del := s.base.BuildDeleteTxItem(pk, subSK(oldest.ID))
	return d.ID, s.base.TransactWrite(ctx, []types.TransactWriteItem{del, s.base.BuildPutTxItemIfAbsent(item)})
}

// Delete removes one device of the person and frees its slot.
func (s *Store) Delete(ctx context.Context, sub, id string) error {
	pk := userPK(sub)
	counts := countsSK
	del := types.TransactWriteItem{Delete: &types.Delete{
		TableName:           aws.String(s.base.TableName),
		Key:                 map[string]types.AttributeValue{"pk": sAV(pk), "sk": sAV(subSK(id))},
		ConditionExpression: aws.String("attribute_exists(pk)"),
	}}
	count := s.base.BuildRawUpdateTxItem(pk, &counts,
		"SET subscriptions = subscriptions - :one", "subscriptions > :zero", nil,
		map[string]types.AttributeValue{":zero": nAV(0), ":one": nAV(1)})
	err := s.base.TransactWrite(ctx, []types.TransactWriteItem{del, count})
	if dynamo.IsConditionFailed(err) {
		return ErrNotFound
	}
	return err
}
