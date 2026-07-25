package dynamo

import (
	"strings"
	"testing"

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
