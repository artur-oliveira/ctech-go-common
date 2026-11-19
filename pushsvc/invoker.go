package pushsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// LambdaAPI is the slice of the Lambda client the Invoker uses.
type LambdaAPI interface {
	Invoke(ctx context.Context, in *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

// Invoker hands a Job to the push-sender Lambda as an async invoke (Lambda queues it and retries twice).
type Invoker struct {
	client       LambdaAPI
	functionName string
}

// NewInvoker returns an Invoker for the named function.
func NewInvoker(c LambdaAPI, functionName string) *Invoker {
	return &Invoker{client: c, functionName: functionName}
}

// Send hands the job over. Anything but a 202 is a failure to hand off.
func (i *Invoker) Send(ctx context.Context, job Job) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	out, err := i.client.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: aws.String(i.functionName), InvocationType: lambdatypes.InvocationTypeEvent, Payload: body,
	})
	if err != nil {
		return fmt.Errorf("pushsvc: invoking the sender: %w", err)
	}
	if out.StatusCode != http.StatusAccepted {
		return fmt.Errorf("pushsvc: unexpected async invoke status %d", out.StatusCode)
	}
	return nil
}
