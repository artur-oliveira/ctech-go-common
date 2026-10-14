package inter

import (
	"bytes"
	"log/slog"
	"regexp"
	"time"
)

// maxTraceBody bounds one logged response body. Inter's problem bodies are
// small; a statement page is not, and the first bytes already show the shape.
const maxTraceBody = 8 << 10

const redacted = "[redacted]"

var accessTokenValue = regexp.MustCompile(`("access_token"\s*:\s*")[^"]*(")`)

// webhookURLValue matches a registered webhook URL in a response. Those URLs
// carry their own secret (a query hmac on one system, a path segment on
// another), so only scheme and host are kept.
var webhookURLValue = regexp.MustCompile(`("webhookUrl"\s*:\s*"https?://[^/"?]*)[^"]*(")`)

// trace logs one Inter response as received, so a field nobody parses yet (or a
// refusal whose reason the error text drops on purpose) can still be read from
// the log. It never writes a secret this client holds: the integration's
// client_secret and the bearer in use are replaced wherever they appear, and a
// token response's access_token is blanked, and a webhookUrl keeps only its scheme and host.
//
// Off unless WithLogger was given: a library does not log by default.
func (c *Client) trace(cr Credentials, bearer, method, path string, status int, took time.Duration, raw []byte) {
	if c.log == nil {
		return
	}
	body := raw
	truncated := 0
	if len(body) > maxTraceBody {
		truncated = len(body) - maxTraceBody
		body = body[:maxTraceBody]
	}
	body = accessTokenValue.ReplaceAll(body, []byte("${1}"+redacted+"${2}"))
	body = webhookURLValue.ReplaceAll(body, []byte("${1}/"+redacted+"${2}"))
	for _, s := range []string{cr.ClientSecret, bearer} {
		if s != "" {
			body = bytes.ReplaceAll(body, []byte(s), []byte(redacted))
		}
	}
	attrs := []any{"method", method, "path", path, "status", status, "ms", took.Milliseconds(), "body", string(body)}
	if truncated > 0 {
		attrs = append(attrs, "truncated", truncated)
	}
	c.log.Info("inter response", attrs...)
}

// WithLogger turns on the raw response trace (see trace).
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }
