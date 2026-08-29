package email

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
)

type fakeSES struct {
	in *sesv2.SendEmailInput
}

func (f *fakeSES) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.in = in
	return &sesv2.SendEmailOutput{MessageId: aws.String("mid-1")}, nil
}

func TestSendUsesSimpleContent(t *testing.T) {
	f := &fakeSES{}
	c := NewWithAPI(f, "billing@aoctech.app")

	if err := c.Send(context.Background(), "a@b.com", "Assunto", "<p>oi</p>"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := aws.ToString(f.in.FromEmailAddress); got != "billing@aoctech.app" {
		t.Errorf("From = %q", got)
	}
	if got := aws.ToString(f.in.Content.Simple.Subject.Data); got != "Assunto" {
		t.Errorf("Subject = %q", got)
	}
}

// A header value carrying a line break is the whole reason this package
// validates anything, so it is the one behaviour pinned by a test.
func TestSendRejectsHeaderInjection(t *testing.T) {
	f := &fakeSES{}
	c := NewWithAPI(f, "billing@aoctech.app")

	err := c.Send(context.Background(), "a@b.com", "Assunto\r\nBcc: outro@x.com", "<p>oi</p>")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if f.in != nil {
		t.Fatal("nothing may be sent after a refusal")
	}
}

func TestSendRawBuildsHeadersAndReturnsMessageID(t *testing.T) {
	f := &fakeSES{}
	c := NewWithAPI(f, "billing@aoctech.app")

	id, err := c.SendRaw(context.Background(), Raw{
		To: "a@b.com", Subject: "Re: ticket", HTML: "<p>oi</p>", InReplyTo: "<root@ses>",
	})
	if err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	if id != "mid-1" {
		t.Errorf("MessageId = %q", id)
	}
	raw := string(f.in.Content.Raw.Data)
	for _, want := range []string{"From: billing@aoctech.app", "To: a@b.com", "In-Reply-To: <root@ses>", "<p>oi</p>"} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw message missing %q:\n%s", want, raw)
		}
	}
}
