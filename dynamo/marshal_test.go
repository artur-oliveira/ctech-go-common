package dynamo

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMarshalMapOmitNull(t *testing.T) {
	t.Run("top-level nulls omitted", func(t *testing.T) {
		in := map[string]any{
			"name":  "Vasilhame",
			"cest":  nil,
			"value": "20.00",
			"empty": "",
		}
		out, err := MarshalMapOmitNull(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := out["cest"]; ok {
			t.Errorf("expected null attribute 'cest' to be omitted, got %#v", out["cest"])
		}
		if _, ok := out["name"]; !ok {
			t.Errorf("expected 'name' to be present")
		}
		if s, ok := out["empty"].(*types.AttributeValueMemberS); !ok || s.Value != "" {
			t.Errorf("expected empty string to be preserved, got %#v", out["empty"])
		}
	})

	t.Run("nested nulls omitted", func(t *testing.T) {
		in := map[string]any{
			"name": "X",
			"addr": map[string]any{"city": "POA", "complement": nil},
			"list": []any{map[string]any{"x": "1", "y": nil}},
		}
		out, err := MarshalMapOmitNull(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		addrAV, ok := out["addr"].(*types.AttributeValueMemberM)
		if !ok {
			t.Fatalf("expected 'addr' to be a map, got %T", out["addr"])
		}
		if _, exists := addrAV.Value["complement"]; exists {
			t.Errorf("expected nested null 'addr.complement' to be omitted")
		}
		if _, exists := addrAV.Value["city"]; !exists {
			t.Errorf("expected 'addr.city' to be present")
		}

		listAV, ok := out["list"].(*types.AttributeValueMemberL)
		if !ok {
			t.Fatalf("expected 'list' to be a list, got %T", out["list"])
		}
		if len(listAV.Value) == 0 {
			t.Fatalf("expected list to have one element")
		}
		elemAV, ok := listAV.Value[0].(*types.AttributeValueMemberM)
		if !ok {
			t.Fatalf("expected list element to be a map, got %T", listAV.Value[0])
		}
		if _, exists := elemAV.Value["y"]; exists {
			t.Errorf("expected nested null 'list[0].y' to be omitted")
		}
		if _, exists := elemAV.Value["x"]; !exists {
			t.Errorf("expected 'list[0].x' to be present")
		}
	})
}
