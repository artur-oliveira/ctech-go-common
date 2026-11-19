package pushsvc

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/dynamo"
)

const reminderPrefix = "REMIND#"

// Reminder is one scheduled push of one person: a kind the product names ("streak", "pending"), whether it is on, the
// 15-minute slot it goes out in, and the last civil day it was sent.
type Reminder struct {
	Sub         string
	Kind        string
	Enabled     bool
	Slot        int
	LastSentDay string
}

type reminderRow struct {
	Enabled     bool   `dynamodbav:"enabled"`
	Slot        int    `dynamodbav:"slot"`
	LastSentDay string `dynamodbav:"last_sent_day"`
}

func reminderSK(kind string) string { return reminderPrefix + kind }
func slotKey(slot int) string       { return "SLOT#" + strconv.Itoa(slot) }

// ReminderStore keeps reminders in the product's `{prefix}_push` table. Only an enabled reminder carries the slot-index
// keys, so the index holds exactly what a tick has to look at.
type ReminderStore struct{ base dynamo.Base }

// NewReminderStore returns a ReminderStore over `{tablePrefix}_push`.
func NewReminderStore(db *dynamodb.Client, tablePrefix string) *ReminderStore {
	return &ReminderStore{base: dynamo.NewBase(db, tablePrefix, TableName)}
}

func (s *ReminderStore) key(sub, kind string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": sAV(userPK(sub)), "sk": sAV(reminderSK(kind))}
}

// Put sets whether a reminder is on and its slot. It keeps last_sent_day, so moving the time cannot send twice a day.
func (s *ReminderStore) Put(ctx context.Context, r Reminder) error {
	if r.Slot < 0 || r.Slot >= SlotsPerDay {
		return fmt.Errorf("pushsvc: slot %d out of range", r.Slot)
	}
	in := &dynamodb.UpdateItemInput{
		TableName:                aws.String(s.base.TableName),
		Key:                      s.key(r.Sub, r.Kind),
		ExpressionAttributeNames: map[string]string{"#e": "enabled", "#s": "slot"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":e": &types.AttributeValueMemberBOOL{Value: r.Enabled}, ":s": nAV(r.Slot),
		},
	}
	if r.Enabled {
		in.UpdateExpression = aws.String("SET #e = :e, #s = :s, slot_key = :sk, kind_sub = :ks")
		in.ExpressionAttributeValues[":sk"] = sAV(slotKey(r.Slot))
		in.ExpressionAttributeValues[":ks"] = sAV(r.Kind + "#" + r.Sub)
	} else {
		in.UpdateExpression = aws.String("SET #e = :e, #s = :s REMOVE slot_key, kind_sub")
	}
	_, err := s.base.UpdateItemRaw(ctx, in)
	return err
}

// MoveSlot moves an enabled reminder from one slot to another and reports whether it did. The write is conditional on the
// reminder still being enabled and still in `from`: a player who switched it off (or moved it) while a tick was working
// is not overruled, so a background move can never resurrect an opt-out.
func (s *ReminderStore) MoveSlot(ctx context.Context, sub, kind string, from, to int) (bool, error) {
	if to < 0 || to >= SlotsPerDay {
		return false, fmt.Errorf("pushsvc: slot %d out of range", to)
	}
	_, err := s.base.UpdateItemRaw(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String(s.base.TableName),
		Key:                      s.key(sub, kind),
		UpdateExpression:         aws.String("SET #s = :to, slot_key = :sk"),
		ConditionExpression:      aws.String("attribute_exists(pk) AND #e = :on AND #s = :from"),
		ExpressionAttributeNames: map[string]string{"#e": "enabled", "#s": "slot"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":to": nAV(to), ":from": nAV(from), ":sk": sAV(slotKey(to)), ":on": &types.AttributeValueMemberBOOL{Value: true},
		},
	})
	if dynamo.IsConditionFailed(err) {
		return false, nil
	}
	return err == nil, err
}

// Get reads one reminder; ok is false when the person never set it.
func (s *ReminderStore) Get(ctx context.Context, sub, kind string) (Reminder, bool, error) {
	it, err := s.base.GetItem(ctx, userPK(sub), reminderSK(kind))
	if err != nil || it == nil {
		return Reminder{}, false, err
	}
	var row reminderRow
	if err := attributevalue.UnmarshalMap(it, &row); err != nil {
		return Reminder{}, false, err
	}
	return Reminder{Sub: sub, Kind: kind, Enabled: row.Enabled, Slot: row.Slot, LastSentDay: row.LastSentDay}, true, nil
}

// ListSlot returns one page of the enabled reminders of a slot. Only Sub, Kind, Enabled and Slot are filled (the index
// is keys only); a tick that needs more calls Get. next is nil on the last page; a short page is not the end.
func (s *ReminderStore) ListSlot(ctx context.Context, slot, limit int, cursor map[string]types.AttributeValue) ([]Reminder, map[string]types.AttributeValue, error) {
	res, err := s.base.Query(ctx, dynamo.QueryOpts{
		PK: slotKey(slot), PKField: "slot_key", IndexName: IndexSlot, Limit: limit, ExclusiveStartKey: cursor,
	})
	if err != nil {
		return nil, nil, err
	}
	out := make([]Reminder, 0, len(res.Items))
	for _, it := range res.Items {
		pk, _ := it["pk"].(*types.AttributeValueMemberS)
		sk, _ := it["sk"].(*types.AttributeValueMemberS)
		if pk == nil || sk == nil || !strings.HasPrefix(pk.Value, userPrefix) || !strings.HasPrefix(sk.Value, reminderPrefix) {
			continue
		}
		out = append(out, Reminder{
			Sub: strings.TrimPrefix(pk.Value, userPrefix), Kind: strings.TrimPrefix(sk.Value, reminderPrefix), Enabled: true, Slot: slot,
		})
	}
	return out, res.LastEvaluatedKey, nil
}

// MarkSent records that the reminder went out on `day` and reports whether this call was the one: false means it was
// already sent that day, or the reminder no longer exists. Call it before sending; at most one send per day wins.
func (s *ReminderStore) MarkSent(ctx context.Context, sub, kind, day string) (bool, error) {
	_, err := s.base.UpdateItemRaw(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.base.TableName),
		Key:                       s.key(sub, kind),
		UpdateExpression:          aws.String("SET last_sent_day = :d"),
		ConditionExpression:       aws.String("attribute_exists(pk) AND (attribute_not_exists(last_sent_day) OR last_sent_day <> :d)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":d": sAV(day)},
	})
	if dynamo.IsConditionFailed(err) {
		return false, nil
	}
	return err == nil, err
}
