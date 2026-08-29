// Package email is the shared SESv2 transport for the family's transactional
// mail.
//
// It carries no templates and no product vocabulary: every service still owns
// what it says and to whom. What was duplicated — building a client, sending
// one HTML message, and building an RFC 5322 message when a header has to be
// set — is here, and that is the whole of it. A shared package that knew about
// invoices would be a notification service, which is a much larger decision
// than removing a copy of ten lines.
package email

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"gopkg.aoctech.app/api-commons/awsconfig"
)

// API is the subset of *sesv2.Client this package calls. Narrowed to an
// interface so a caller's tests can capture what would have been sent instead
// of holding real credentials — a mail path that can only be exercised against
// real SES is a mail path nobody exercises.
type API interface {
	SendEmail(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// Client sends through SESv2 from one verified address.
type Client struct {
	ses  API
	from string
}

// New builds a client against the real SESv2 endpoint.
//
// An empty `from` is refused rather than defaulted: SES rejects the send at
// delivery time, which turns a configuration mistake into an incident hours
// later instead of a start-up failure.
func New(ctx context.Context, region, from string) (*Client, error) {
	if strings.TrimSpace(from) == "" {
		return nil, errors.New("email: a sender address is required")
	}
	cfg, err := awsconfig.Load(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("email: loading AWS config: %w", err)
	}
	return &Client{ses: sesv2.NewFromConfig(cfg), from: from}, nil
}

// NewWithAPI wraps an already-built SES client (or a fake).
func NewWithAPI(api API, from string) *Client { return &Client{ses: api, from: from} }

// From is the address this client sends as.
func (c *Client) From() string { return c.from }

// Send delivers one HTML message.
func (c *Client) Send(ctx context.Context, to, subject, htmlBody string) error {
	if strings.TrimSpace(to) == "" {
		return errors.New("email: no recipient")
	}
	if err := noHeaderInjection(to, subject); err != nil {
		return err
	}
	_, err := c.ses.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(c.from),
		Destination:      &sestypes.Destination{ToAddresses: []string{to}},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{Data: aws.String(subject), Charset: aws.String("UTF-8")},
				Body: &sestypes.Body{
					Html: &sestypes.Content{Data: aws.String(htmlBody), Charset: aws.String("UTF-8")},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("email: sending to %s: %w", to, err)
	}
	return nil
}

// Raw is a message that needs headers SESv2's simple content cannot express —
// today, threading.
type Raw struct {
	To         string
	Subject    string
	HTML       string
	InReplyTo  string
	References string
}

// SendRaw builds an RFC 5322 message and returns the assigned Message-ID with
// no angle brackets, which is the form a caller stores and replies to.
func (c *Client) SendRaw(ctx context.Context, m Raw) (string, error) {
	if strings.TrimSpace(m.To) == "" {
		return "", errors.New("email: no recipient")
	}
	// Every header value must be one line. A caller is expected to have
	// rejected line breaks further upstream; one that reaches here would let
	// its source append arbitrary headers to the message.
	if err := noHeaderInjection(m.To, m.Subject, m.InReplyTo, m.References); err != nil {
		return "", err
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", c.from)
	fmt.Fprintf(&buf, "To: %s\r\n", m.To)
	fmt.Fprintf(&buf, "Subject: %s\r\n", m.Subject)
	if m.InReplyTo != "" {
		fmt.Fprintf(&buf, "In-Reply-To: %s\r\n", m.InReplyTo)
	}
	if m.References != "" {
		fmt.Fprintf(&buf, "References: %s\r\n", m.References)
	}
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
	buf.WriteString(m.HTML)

	out, err := c.ses.SendEmail(ctx, &sesv2.SendEmailInput{
		Content: &sestypes.EmailContent{Raw: &sestypes.RawMessage{Data: buf.Bytes()}},
	})
	if err != nil {
		return "", fmt.Errorf("email: sending to %s: %w", m.To, err)
	}
	return aws.ToString(out.MessageId), nil
}

func noHeaderInjection(values ...string) error {
	for _, v := range values {
		if strings.ContainsAny(v, "\r\n") {
			return errors.New("email: header value must not contain line breaks")
		}
	}
	return nil
}
