//go:build integration

package pushsvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// newTestDB creates a fresh `push` table (pk/sk, endpoint-index, slot-index) on DynamoDB Local and returns the client and
// the table prefix. The table is dropped when the test ends.
//
//	docker compose -f docker-compose.test.yml up -d
//	DYNAMODB_ENDPOINT=http://localhost:8131 go test -tags integration -race -count=1 ./pushsvc/
func newTestDB(t *testing.T) (*dynamodb.Client, string) {
	t.Helper()
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYNAMODB_ENDPOINT not set")
	}
	db := dynamodb.New(dynamodb.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	prefix := fmt.Sprintf("it%d", time.Now().UnixNano())
	name := prefix + "_push"
	s := func(n string) types.AttributeDefinition {
		return types.AttributeDefinition{AttributeName: aws.String(n), AttributeType: types.ScalarAttributeTypeS}
	}
	key := func(n string, kt types.KeyType) types.KeySchemaElement {
		return types.KeySchemaElement{AttributeName: aws.String(n), KeyType: kt}
	}
	_, err := db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:            aws.String(name),
		BillingMode:          types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{s("pk"), s("sk"), s("endpoint_id"), s("slot_key"), s("kind_sub")},
		KeySchema:            []types.KeySchemaElement{key("pk", types.KeyTypeHash), key("sk", types.KeyTypeRange)},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName:  aws.String(IndexEndpoint),
				KeySchema:  []types.KeySchemaElement{key("endpoint_id", types.KeyTypeHash), key("pk", types.KeyTypeRange)},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			},
			{
				IndexName:  aws.String(IndexSlot),
				KeySchema:  []types.KeySchemaElement{key("slot_key", types.KeyTypeHash), key("kind_sub", types.KeyTypeRange)},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(name)})
	})
	return db, prefix
}
