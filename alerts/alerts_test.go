package alerts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

type fakeSNS struct{ in *sns.PublishInput }

func (f *fakeSNS) Publish(_ context.Context, in *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
	f.in = in
	return &sns.PublishOutput{}, nil
}

func TestAlertCarriesIdentityAndCause(t *testing.T) {
	f := &fakeSNS{}
	c := &Client{sns: f, topicARN: "arn:aws:sns:us-east-1:1:alerts", service: "billing", environment: "prod"}

	c.Alert(context.Background(), Alert{Job: "sweep", Summary: "1 subscription failed", Err: errors.New("boom")})

	if got := aws.ToString(f.in.Subject); got != "[billing/prod] sweep" {
		t.Errorf("Subject = %q", got)
	}
	body := aws.ToString(f.in.Message)
	for _, want := range []string{"job: sweep", "1 subscription failed", "error: boom"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// A publish that fails must not fail the caller: the job already had one
// problem and reporting it is not allowed to become a second.
func TestAlertSwallowsPublishFailure(t *testing.T) {
	c := &Client{sns: failingSNS{}, topicARN: "arn", service: "billing", environment: "prod"}
	c.Alert(context.Background(), Alert{Job: "deliver", Summary: "x"})
}

type failingSNS struct{}

func (failingSNS) Publish(context.Context, *sns.PublishInput, ...func(*sns.Options)) (*sns.PublishOutput, error) {
	return nil, errors.New("no topic")
}

func TestNewWithoutTopicIsNop(t *testing.T) {
	p, err := New(context.Background(), "us-east-1", "  ", "billing", "dev")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.(Nop); !ok {
		t.Fatalf("want Nop, got %T", p)
	}
}
