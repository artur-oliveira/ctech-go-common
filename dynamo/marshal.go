package dynamo

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// deleteNulls recursively removes null attributes from a DynamoDB attribute
// map, descending into nested maps and list elements.
func deleteNulls(av map[string]types.AttributeValue) {
	for k, val := range av {
		switch v := val.(type) {
		case *types.AttributeValueMemberNULL:
			delete(av, k)
		case *types.AttributeValueMemberM:
			deleteNulls(v.Value)
		case *types.AttributeValueMemberL:
			for _, elem := range v.Value {
				if m, ok := elem.(*types.AttributeValueMemberM); ok {
					deleteNulls(m.Value)
				}
			}
		}
	}
}

// MarshalMapOmitNull marshals v into a DynamoDB attribute map, omitting any
// attribute whose value is null (recursively, including nested maps and list
// elements). This keeps stored items small without changing the API contract —
// reads reconstruct absent attributes as null.
func MarshalMapOmitNull(v any) (map[string]types.AttributeValue, error) {
	av, err := attributevalue.MarshalMap(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	deleteNulls(av)
	return av, nil
}
