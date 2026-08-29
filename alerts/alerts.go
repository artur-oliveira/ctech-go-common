// Package alerts publishes operator-visible failures to an SNS topic.
//
// It exists because CloudWatch alarms are priced per alarm per month and the
// family needs dozens of them to say one thing: "this job did not do its work".
// A process that already knows it failed can say so itself for the price of an
// SNS publish, and SNS e-mail is free at this volume. What is given up is the
// alarm that fires when nothing publishes at all — a process that never starts
// sends no failure — so a caller that needs liveness has to assert it from the
// other side (a later job checking a marker), not from this package.
//
// Nothing here may fail a caller. An alert is a side effect of an error that
// already happened; returning a second error would make the reporting path a
// new way for the job to break.
package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"gopkg.aoctech.app/api-commons/awsconfig"
)

// Publisher is what a job holds. Nop is a valid one, which is what makes
// "alerting is not configured here" a deployment choice rather than a nil check
// at every call site.
type Publisher interface {
	// Alert reports one failure. It never returns an error: a failure to
	// report is logged and swallowed.
	Alert(ctx context.Context, a Alert)
}

// Alert is one message. Subject is what arrives in a mail client's list, so it
// carries the identity — service, environment, job — and the body carries the
// detail.
type Alert struct {
	// Job names the unit of work that failed: "sweep", "dunning", "deliver".
	Job string
	// Summary is one line, in the imperative facts of what went wrong.
	Summary string
	// Err is optional and appended verbatim.
	Err error
	// Detail is optional free text — counts, ids, whatever makes the mail
	// actionable without opening a log.
	Detail string
}

type api interface {
	Publish(ctx context.Context, in *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// Client publishes to one topic.
type Client struct {
	sns         api
	topicARN    string
	service     string
	environment string
}

// New builds a publisher. An empty topic ARN yields Nop rather than an error:
// a local run and a test deployment have nobody to page, and refusing to start
// over it would be worse than staying quiet.
func New(ctx context.Context, region, topicARN, service, environment string) (Publisher, error) {
	if strings.TrimSpace(topicARN) == "" {
		return Nop{}, nil
	}
	cfg, err := awsconfig.Load(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("alerts: loading AWS config: %w", err)
	}
	return &Client{
		sns:         sns.NewFromConfig(cfg),
		topicARN:    topicARN,
		service:     service,
		environment: environment,
	}, nil
}

func (c *Client) Alert(ctx context.Context, a Alert) {
	subject := fmt.Sprintf("[%s/%s] %s", c.service, c.environment, a.Job)
	// SNS refuses a subject over 100 characters, and refusing the publish would
	// lose the alert over its title.
	if len(subject) > 100 {
		subject = subject[:100]
	}

	var body strings.Builder
	fmt.Fprintf(&body, "service: %s\nenvironment: %s\njob: %s\n\n%s\n", c.service, c.environment, a.Job, a.Summary)
	if a.Detail != "" {
		fmt.Fprintf(&body, "\n%s\n", a.Detail)
	}
	if a.Err != nil {
		fmt.Fprintf(&body, "\nerror: %v\n", a.Err)
	}

	if _, err := c.sns.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(c.topicARN),
		Subject:  aws.String(subject),
		Message:  aws.String(body.String()),
	}); err != nil {
		slog.ErrorContext(ctx, "alert publish failed", "job", a.Job, "error", err)
	}
}

// Nop discards alerts. It is the configured-off implementation and the one a
// test uses.
type Nop struct{}

func (Nop) Alert(context.Context, Alert) {}
