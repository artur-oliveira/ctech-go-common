package dynamo

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/problem"
)

// Page-size policy shared by every list endpoint.
const (
	DefaultPageLimit = 25
	MaxPageLimit     = 100
)

const (
	cursorTypeString = "S"
	cursorTypeNumber = "N"
	cursorTypeBinary = "B"

	// ownerTagBytes is how much of sha256(owner) rides in the cursor: enough
	// to catch a cursor replayed under another owner, short enough to keep
	// the cursor small. The tag hides the raw owner id from the client.
	ownerTagBytes = 8

	invalidCursorDetail = "cursor inválido"
)

// ErrInvalidCursor is the cause carried by the 400 problem DecodeCursor
// returns for a malformed cursor or one issued for a different owner.
var ErrInvalidCursor = errors.New("dynamo: invalid cursor")

// ParseLimit reads a `limit` query param: DefaultPageLimit when empty,
// non-numeric or <= 0, capped at MaxPageLimit. Without the cap a client picks
// the size of every read.
func ParseLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultPageLimit
	}
	return ClampLimit(n)
}

// ClampLimit applies the same policy to an already-parsed number.
func ClampLimit(n int) int {
	if n <= 0 {
		return DefaultPageLimit
	}
	return min(n, MaxPageLimit)
}

// cursorAttr keeps the DynamoDB key type, so a numeric sort key round-trips as
// a number (a plain JSON map would turn it into a float64 and lose precision).
type cursorAttr struct {
	T string `json:"t"`
	V string `json:"v"`
}

type cursorPayload struct {
	Key   map[string]cursorAttr `json:"k"`
	Prev  map[string]cursorAttr `json:"p,omitempty"` // only the previous page's key, never the previous cursor (it would nest and grow every page)
	Owner string                `json:"o,omitempty"`
}

func ownerTag(owner string) string {
	if owner == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:ownerTagBytes])
}

func keyToPlain(key map[string]types.AttributeValue) (map[string]cursorAttr, bool) {
	out := make(map[string]cursorAttr, len(key))
	for k, v := range key {
		switch a := v.(type) {
		case *types.AttributeValueMemberS:
			out[k] = cursorAttr{cursorTypeString, a.Value}
		case *types.AttributeValueMemberN:
			out[k] = cursorAttr{cursorTypeNumber, a.Value}
		case *types.AttributeValueMemberB:
			out[k] = cursorAttr{cursorTypeBinary, base64.StdEncoding.EncodeToString(a.Value)}
		default:
			return nil, false // not a key type
		}
	}
	return out, true
}

func plainToKey(plain map[string]cursorAttr) (map[string]types.AttributeValue, bool) {
	out := make(map[string]types.AttributeValue, len(plain))
	for k, a := range plain {
		switch a.T {
		case cursorTypeString:
			out[k] = &types.AttributeValueMemberS{Value: a.V}
		case cursorTypeNumber:
			out[k] = &types.AttributeValueMemberN{Value: a.V}
		case cursorTypeBinary:
			b, err := base64.StdEncoding.DecodeString(a.V)
			if err != nil {
				return nil, false
			}
			out[k] = &types.AttributeValueMemberB{Value: b}
		default:
			return nil, false
		}
	}
	return out, true
}

func encodePayload(p cursorPayload) string {
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// EncodeCursor renders a continuation key (a Query's LastEvaluatedKey) as an
// opaque URL-safe string bound to owner (an org/user/tenant id; "" for none).
// An empty key, or one with a non-key attribute type, yields "" (no next
// page): a truncated cursor would silently restart the listing.
//
// The owner tag is defense in depth, NOT a signature: DynamoDB already rejects
// a start key outside the queried partition, and the tag makes a cursor
// replayed under another owner a clean 400. shortcut: unsigned, add an HMAC if
// cursors ever carry data the client must not be able to forge.
func EncodeCursor(key map[string]types.AttributeValue, owner string) string {
	if len(key) == 0 {
		return ""
	}
	plain, ok := keyToPlain(key)
	if !ok {
		return ""
	}
	return encodePayload(cursorPayload{Key: plain, Owner: ownerTag(owner)})
}

func decodePayload(cursor, owner string) (*cursorPayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, err
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Key) == 0 || p.Owner != ownerTag(owner) {
		return nil, ErrInvalidCursor
	}
	return &p, nil
}

// DecodeCursor parses a cursor into an ExclusiveStartKey. "" means "from the
// start" (nil, nil). A malformed cursor, or one issued for another owner,
// fails closed with a 400 *problem.Problem (Cause() is ErrInvalidCursor):
// ignoring it would restart the listing and hand the client page 1 as if it
// were page N.
func DecodeCursor(cursor, owner string) (map[string]types.AttributeValue, error) {
	if cursor == "" {
		return nil, nil
	}
	p, err := decodePayload(cursor, owner)
	if err != nil {
		return nil, problem.BadRequest(invalidCursorDetail).WithCause(ErrInvalidCursor)
	}
	key, ok := plainToKey(p.Key)
	if !ok {
		return nil, problem.BadRequest(invalidCursorDetail).WithCause(ErrInvalidCursor)
	}
	return key, nil
}

// Page is the list envelope: {"data": [...], "next_cursor": "..."|null}.
// next_cursor null is the only end-of-list signal (a short page is not).
type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// NewPage builds the envelope; Data is [] (never null) for an empty page.
func NewPage[T any](items []T, lastEvaluatedKey map[string]types.AttributeValue, owner string) Page[T] {
	if items == nil {
		items = []T{}
	}
	var next *string
	if c := EncodeCursor(lastEvaluatedKey, owner); c != "" {
		next = &c
	}
	return Page[T]{Data: items, NextCursor: next}
}

// PageWithPrevious is the envelope for lists that also page backwards.
type PageWithPrevious[T any] struct {
	Data           []T     `json:"data"`
	NextCursor     *string `json:"next_cursor"`
	HasNext        bool    `json:"has_next"`
	PreviousCursor *string `json:"previous_cursor"`
	HasPrevious    bool    `json:"has_previous"`
}

// NewPageWithPrevious builds the bidirectional envelope. incomingCursor is the
// (already DecodeCursor-validated) cursor of THIS request. The next cursor
// remembers the key this page started from, so the following page can send the
// client back here; previous_cursor points at the page before this one. It is
// null on the first and second page: the page before the second is the first,
// reached by sending no cursor, which has_previous (true from page two on)
// tells the client.
func NewPageWithPrevious[T any](items []T, lastEvaluatedKey map[string]types.AttributeValue, incomingCursor, owner string) PageWithPrevious[T] {
	if items == nil {
		items = []T{}
	}
	var prevKey map[string]cursorAttr // incoming's own start key, for the next cursor's "p"
	var prevCursor *string
	if incomingCursor != "" {
		if in, err := decodePayload(incomingCursor, owner); err == nil {
			prevKey = in.Key
			if len(in.Prev) > 0 {
				c := encodePayload(cursorPayload{Key: in.Prev, Owner: ownerTag(owner)})
				prevCursor = &c
			}
		}
	}
	var next *string
	if plain, ok := keyToPlain(lastEvaluatedKey); ok && len(plain) > 0 {
		c := encodePayload(cursorPayload{Key: plain, Prev: prevKey, Owner: ownerTag(owner)})
		next = &c
	}
	return PageWithPrevious[T]{
		Data:           items,
		NextCursor:     next,
		HasNext:        next != nil,
		PreviousCursor: prevCursor,
		HasPrevious:    incomingCursor != "",
	}
}
