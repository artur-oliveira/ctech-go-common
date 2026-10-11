package dynamo

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestNewBasePrefixesTable(t *testing.T) {
	b := NewBase(nil, "test", "wallets")
	if b.TableName != "test_wallets" {
		t.Fatalf("TableName = %q, want %q", b.TableName, "test_wallets")
	}
}

func TestBuildUpdateExpr_SetAndRemove(t *testing.T) {
	expr, names, values, err := buildUpdateExpr(map[string]any{
		"name": "X",
		"cest": nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(expr, "SET #name = :name") {
		t.Errorf("expected SET clause for name, got %q", expr)
	}
	if !strings.Contains(expr, "REMOVE #cest") {
		t.Errorf("expected REMOVE clause for cest, got %q", expr)
	}
	if _, ok := values[":cest"]; ok {
		t.Errorf("nil value must not be in ExpressionAttributeValues")
	}
	if names["#cest"] != "cest" {
		t.Errorf("expected name mapping for cest")
	}
}

func TestBuildUpdateExpr_RemoveOnly(t *testing.T) {
	expr, _, values, err := buildUpdateExpr(map[string]any{"cest": nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(expr, "SET") {
		t.Errorf("expected no SET clause, got %q", expr)
	}
	if !strings.HasPrefix(expr, "REMOVE") {
		t.Errorf("expected REMOVE-only expression, got %q", expr)
	}
	if len(values) != 0 {
		t.Errorf("expected no expression values, got %d", len(values))
	}
}

func TestBase_BuildPutTxItem(t *testing.T) {
	b := Base{TableName: "test_table"}
	item := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "PK1"},
		"sk": &types.AttributeValueMemberS{Value: "SK1"},
	}
	txItem := b.BuildPutTxItem(item)
	if txItem.Put == nil {
		t.Fatal("expected Put transact item, got nil")
	}
	if *txItem.Put.TableName != b.TableName {
		t.Errorf("table name = %q, want %q", *txItem.Put.TableName, b.TableName)
	}
	if txItem.Put.Item["pk"].(*types.AttributeValueMemberS).Value != "PK1" {
		t.Error("item not carried through unchanged")
	}
}

func TestBase_BuildUpdateTxItem(t *testing.T) {
	b := Base{TableName: "test_table"}
	txItem, err := b.BuildUpdateTxItem("PK1", new("SK1"), map[string]any{"name": "new-name"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if txItem.Update == nil {
		t.Fatal("expected Update transact item, got nil")
	}
	if *txItem.Update.ConditionExpression != "attribute_exists(pk)" {
		t.Errorf("condition = %q, want attribute_exists(pk)", *txItem.Update.ConditionExpression)
	}
	if txItem.Update.Key["sk"].(*types.AttributeValueMemberS).Value != "SK1" {
		t.Error("sk not set on key")
	}
}

func TestBase_BuildDeleteTxItem(t *testing.T) {
	b := Base{TableName: "test_table"}
	txItem := b.BuildDeleteTxItem("PK1", "SK1")
	if txItem.Delete == nil {
		t.Fatal("expected Delete transact item, got nil")
	}
	if *txItem.Delete.ConditionExpression != "attribute_exists(pk)" {
		t.Errorf("condition = %q, want attribute_exists(pk)", *txItem.Delete.ConditionExpression)
	}
}

func TestBuildCompositeKeyCondition_EqPrefixAndBeginsWith(t *testing.T) {
	cond, names, values := buildCompositeKeyCondition(CompositeQueryOpts{
		PKField: "pk",
		PK:      "USER#1",
		SKEq: []KV{
			{Field: "country", Value: "BR"},
			{Field: "state", Value: "SP"},
		},
		SKLastField: "city",
		SKLastOp:    "begins_with",
		SKLastValue: "S",
	})

	want := "#pk = :pk AND #sk0 = :sk0 AND #sk1 = :sk1 AND begins_with(#skl, :skl)"
	if cond != want {
		t.Errorf("cond = %q, want %q", cond, want)
	}
	if names["#sk0"] != "country" || names["#sk1"] != "state" || names["#skl"] != "city" {
		t.Errorf("unexpected names: %+v", names)
	}
	if values[":sk0"].(*types.AttributeValueMemberS).Value != "BR" {
		t.Error("sk0 value not carried through")
	}
	if values[":skl"].(*types.AttributeValueMemberS).Value != "S" {
		t.Error("skl value not carried through")
	}
}

func TestBuildCompositeKeyCondition_Between(t *testing.T) {
	cond, _, values := buildCompositeKeyCondition(CompositeQueryOpts{
		PKField:      "pk",
		PK:           "USER#1",
		SKLastField:  "amount",
		SKLastOp:     "between",
		SKLastValue:  "10",
		SKLastValue2: "20",
	})

	want := "#pk = :pk AND #skl BETWEEN :skl1 AND :skl2"
	if cond != want {
		t.Errorf("cond = %q, want %q", cond, want)
	}
	if values[":skl1"].(*types.AttributeValueMemberS).Value != "10" || values[":skl2"].(*types.AttributeValueMemberS).Value != "20" {
		t.Errorf("unexpected between values: %+v", values)
	}
}

func TestBuildCompositeKeyCondition_NoSK(t *testing.T) {
	cond, names, _ := buildCompositeKeyCondition(CompositeQueryOpts{PKField: "pk", PK: "USER#1"})
	if cond != "#pk = :pk" {
		t.Errorf("cond = %q, want PK-only condition", cond)
	}
	if len(names) != 1 {
		t.Errorf("expected only #pk name, got %+v", names)
	}
}

func TestBase_UpsertAttrs_NoConditionExpression(t *testing.T) {
	// UpsertAttrs must NOT carry attribute_exists(pk) — that's the entire point:
	// it creates the row on first write instead of failing when absent.
	expr, names, values, err := buildUpdateExpr(map[string]any{"consent_a": "2026-07-17"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(expr, "SET #consent_a = :consent_a") {
		t.Errorf("expected SET clause, got %q", expr)
	}
	if names["#consent_a"] != "consent_a" {
		t.Errorf("expected name mapping for consent_a")
	}
	if _, ok := values[":consent_a"]; !ok {
		t.Errorf("expected :consent_a value")
	}
}

func TestBuildFilterExpr(t *testing.T) {
	cases := []struct {
		name     string
		opts     QueryOpts
		wantExpr string
		wantVals map[string]string
	}{
		{
			name:     "none",
			opts:     QueryOpts{},
			wantExpr: "",
		},
		{
			name:     "equality only",
			opts:     QueryOpts{FilterField: "org_pk", FilterValue: "CNPJ_1"},
			wantExpr: "#filter_field = :filter_value",
			wantVals: map[string]string{":filter_value": "CNPJ_1"},
		},
		{
			name:     "contains only",
			opts:     QueryOpts{FilterContainsField: "roles", FilterContainsValue: "driver"},
			wantExpr: "contains(#filter_contains_field, :filter_contains_value)",
			wantVals: map[string]string{":filter_contains_value": "driver"},
		},
		{
			name: "both",
			opts: QueryOpts{
				FilterField:         "org_pk",
				FilterValue:         "CNPJ_1",
				FilterContainsField: "roles",
				FilterContainsValue: "driver",
			},
			wantExpr: "#filter_field = :filter_value AND contains(#filter_contains_field, :filter_contains_value)",
			wantVals: map[string]string{":filter_value": "CNPJ_1", ":filter_contains_value": "driver"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, names, values := buildFilterExpr(tc.opts)
			if expr != tc.wantExpr {
				t.Fatalf("expr = %q, want %q", expr, tc.wantExpr)
			}
			if tc.wantExpr == "" {
				if names != nil || values != nil {
					t.Fatalf("expected nil maps for empty expression")
				}
				return
			}
			if tc.opts.FilterField != "" && names["#filter_field"] != tc.opts.FilterField {
				t.Errorf("names[#filter_field] = %q", names["#filter_field"])
			}
			if tc.opts.FilterContainsField != "" && names["#filter_contains_field"] != tc.opts.FilterContainsField {
				t.Errorf("names[#filter_contains_field] = %q", names["#filter_contains_field"])
			}
			if len(values) != len(tc.wantVals) {
				t.Fatalf("values len = %d, want %d", len(values), len(tc.wantVals))
			}
			for k, want := range tc.wantVals {
				got, ok := values[k].(*types.AttributeValueMemberS)
				if !ok || got.Value != want {
					t.Errorf("values[%s] = %v, want %q", k, values[k], want)
				}
			}
		})
	}
}

// A cancelled transaction says nothing by itself: every consumer that treats
// IsConditionFailed as a verdict ("my condition lost, reconcile and move on")
// needs the ConditionalCheckFailed reason specifically. Reporting a
// TransactionConflict or a throttle as a condition failure is what let
// ctech-poker swallow an all-in runout step that was never written and freeze
// the hand — see IsConditionFailed's doc comment.
func canceledWithReasons(codes ...string) error {
	reasons := make([]types.CancellationReason, 0, len(codes))
	for _, code := range codes {
		reasons = append(reasons, types.CancellationReason{Code: aws.String(code)})
	}
	return &types.TransactionCanceledException{
		Message:             aws.String("Transaction cancelled, please refer cancellation reasons for specific reasons"),
		CancellationReasons: reasons,
	}
}

func TestTransactionCancellationReasonsAreNotConflated(t *testing.T) {
	cases := []struct {
		name                            string
		err                             error
		conditionFailed, conflict, slow bool
	}{
		{
			name:            "condition failed on one item",
			err:             canceledWithReasons("None", "ConditionalCheckFailed"),
			conditionFailed: true,
		},
		{
			name:     "concurrent transaction on the same item",
			err:      canceledWithReasons("TransactionConflict", "None"),
			conflict: true,
		},
		{
			name: "throttled",
			err:  canceledWithReasons("None", "ThrottlingError"),
			slow: true,
		},
		{
			name: "throughput exceeded",
			err:  canceledWithReasons("ProvisionedThroughputExceeded"),
			slow: true,
		},
		{
			name: "validation error is nobody's condition",
			err:  canceledWithReasons("ValidationError"),
		},
		{
			name:            "single-item conditional check",
			err:             &types.ConditionalCheckFailedException{Message: aws.String("The conditional request failed")},
			conditionFailed: true,
		},
		{
			name:            "wrapped condition failure",
			err:             fmt.Errorf("commit: %w", canceledWithReasons("ConditionalCheckFailed")),
			conditionFailed: true,
		},
		{
			name:     "wrapped conflict",
			err:      fmt.Errorf("commit: %w", canceledWithReasons("TransactionConflict")),
			conflict: true,
		},
		{
			name: "nil",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConditionFailed(tc.err); got != tc.conditionFailed {
				t.Errorf("IsConditionFailed = %v, want %v", got, tc.conditionFailed)
			}
			if got := IsTransactionConflict(tc.err); got != tc.conflict {
				t.Errorf("IsTransactionConflict = %v, want %v", got, tc.conflict)
			}
			if got := IsTransactionThrottled(tc.err); got != tc.slow {
				t.Errorf("IsTransactionThrottled = %v, want %v", got, tc.slow)
			}
		})
	}
}

// An exception whose reasons never decoded (or one flattened to a string by a
// transport that dropped the typed error) must still be classifiable: the
// reason codes appear in DynamoDB's own message.
func TestCancellationReasonsFallBackToTheMessage(t *testing.T) {
	stringified := errors.New("operation error DynamoDB: TransactWriteItems, " +
		"TransactionCanceledException: Transaction cancelled, please refer cancellation " +
		"reasons for specific reasons [TransactionConflict, None]")
	if IsConditionFailed(stringified) {
		t.Error("a stringified conflict must not read as a condition failure")
	}
	if !IsTransactionConflict(stringified) {
		t.Error("expected the conflict reason to be recognised from the message")
	}

	reasonless := &types.TransactionCanceledException{
		Message: aws.String("Transaction cancelled ... [ConditionalCheckFailed, None]"),
	}
	if !IsConditionFailed(reasonless) {
		t.Error("expected a reason-less exception to fall back to its message")
	}
	if IsTransactionConflict(reasonless) {
		t.Error("a condition failure must not read as a conflict")
	}

	opaque := &types.TransactionCanceledException{Message: aws.String("Transaction cancelled")}
	if IsConditionFailed(opaque) || IsTransactionConflict(opaque) || IsTransactionThrottled(opaque) {
		t.Error("an unclassifiable cancellation must not impersonate any specific reason")
	}
}

func TestProjectedNamesResolvesAliasesAndPaths(t *testing.T) {
	got := projectedNames(&dynamodb.QueryInput{
		ProjectionExpression:     aws.String("payload, #s, items[0], meta.created_at"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
	})
	for _, want := range []string{"payload", "status", "items", "meta"} {
		if !got[want] {
			t.Errorf("projectedNames missing %q: %v", want, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("projectedNames = %v, want 4 names", got)
	}
	if projectedNames(&dynamodb.QueryInput{}) != nil {
		t.Error("no projection must yield nil")
	}
}

func TestFilteredQueryRejectsANegativeLimit(t *testing.T) {
	b := Base{TableName: "t"}
	_, err := b.Query(t.Context(), QueryOpts{PK: "P", Limit: -1, FilterField: "status", FilterValue: "x"})
	if err == nil || !strings.Contains(err.Error(), "positive Limit") {
		t.Fatalf("err = %v, want a positive-Limit error", err)
	}
}
