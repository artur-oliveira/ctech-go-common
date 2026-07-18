// Package awsconfig provides the shared AWS SDK v2 config-loading and
// DynamoDB-client bootstrap used across CTech services. Each service still
// owns its own set of AWS clients (S3, SQS, SNS, Lambda, SSM, ...) — only the
// config load and the DynamoDB endpoint-override pattern (for local
// DynamoDB-local development) is common enough to share.
package awsconfig

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// Load resolves AWS credentials via the standard SDK chain (env vars →
// ~/.aws/credentials → EC2 IMDS → ECS task role) for the given region.
func Load(ctx context.Context, region string) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
}

// NewDynamoDBClient builds a DynamoDB client from the given AWS config. A
// non-empty endpointOverride points the client at a local endpoint (e.g.
// DynamoDB-local) instead of the resolved AWS endpoint.
func NewDynamoDBClient(cfg aws.Config, endpointOverride string) *dynamodb.Client {
	var opts []func(*dynamodb.Options)
	if endpointOverride != "" {
		opts = append(opts, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(endpointOverride)
		})
	}
	return dynamodb.NewFromConfig(cfg, opts...)
}
