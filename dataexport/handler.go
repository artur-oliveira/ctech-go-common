package dataexport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// allowedPutURL is defense in depth behind the queue policy (only the account
// topic can publish): the part is personal data, so it only ever goes to
// HTTPS S3, whatever a message says.
func allowedPutURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && strings.HasSuffix(u.Hostname(), ".amazonaws.com")
}

// PartFunc builds the service's part for sub: a zip with JSON (and CSV) files
// and a README (spec E3/E4). It must be idempotent: a redelivery builds again
// and overwrites the same object.
// ponytail: the part is held in memory because a presigned PUT needs a
// Content-Length. Parts are one person's data; stream through a multipart
// upload if one ever outgrows that.
type PartFunc func(ctx context.Context, sub string) ([]byte, error)

// PartStreamFunc is PartFunc for a part that may not fit in memory: it writes the
// zip to w as it reads the person's data, one page at a time. The handler spools
// w to a temporary file and uploads it, because a presigned PUT needs the exact
// Content-Length and S3 refuses a chunked body there. Memory is what one page of
// data holds; the disk is the compressed zip (maxSpooledPart bounds it). Same
// idempotency rule as PartFunc.
type PartStreamFunc func(ctx context.Context, sub string, w io.Writer) error

// maxSpooledPart is the largest part the handler will spool and upload. A single
// presigned PUT carries at most 5 GiB; a part anywhere near it means the data was
// not meant to leave through this door, so the export fails loudly instead of
// filling the instance's disk.
const maxSpooledPart = 1 << 30

// Acker reports the upload to ctech-account.
type Acker interface {
	Send(ctx context.Context, a Ack) error
}

type Handler struct {
	service string
	part    PartFunc
	stream  PartStreamFunc
	http    *http.Client
	acks    Acker
	allow   func(string) bool
	now     func() time.Time
	maxPart int64
}

func NewHandler(service string, part PartFunc, httpClient *http.Client, acks Acker) *Handler {
	// A presigned URL never needs a redirect; following one would send the part elsewhere.
	c := *httpClient
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Handler{service: service, part: part, http: &c, acks: acks, allow: allowedPutURL, now: time.Now, maxPart: maxSpooledPart}
}

// NewStreamHandler is NewHandler for a service whose part is built by streaming
// (PartStreamFunc) instead of returned whole in memory.
func NewStreamHandler(service string, part PartStreamFunc, httpClient *http.Client, acks Acker) *Handler {
	h := NewHandler(service, nil, httpClient, acks)
	h.stream = part
	return h
}

// Handle processes a queue body when it is an export message. handled is false
// for anything else, so erasure.Consumer can carry on with its own types. A
// non-nil error leaves the message for redelivery.
func (h *Handler) Handle(ctx context.Context, body []byte) (handled bool, err error) {
	if !IsExport(body) {
		return false, nil
	}
	m, err := Decode(body)
	if err != nil {
		return true, err
	}
	if m.Service != h.service {
		return true, nil
	}
	if !m.ExpiresAt.After(h.now()) {
		// The URL is dead; account notices the silence and republishes a fresh one.
		slog.WarnContext(ctx, "dataexport: put url expired, dropping", "service", h.service, "request_id", m.RequestID)
		return true, nil
	}
	if !h.allow(m.PutURL) {
		// Never retried: redelivery cannot make a bad URL good.
		slog.ErrorContext(ctx, "dataexport: refusing put url outside https S3", "service", h.service, "request_id", m.RequestID)
		return true, nil
	}
	size, err := h.build(ctx, m)
	if err != nil {
		return true, err
	}
	return true, h.acks.Send(ctx, Ack{RequestID: m.RequestID, Sub: m.Sub, Service: h.service, Result: ResultDone, Size: size, At: h.now().UTC()})
}

// build makes the part and uploads it, returning its size.
func (h *Handler) build(ctx context.Context, m Message) (int64, error) {
	if h.stream != nil {
		return h.buildStreamed(ctx, m)
	}
	data, err := h.part(ctx, m.Sub)
	if err != nil {
		return 0, fmt.Errorf("dataexport: building part for %s: %w", m.RequestID, err)
	}
	if err := h.put(ctx, m.PutURL, data); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (h *Handler) buildStreamed(ctx context.Context, m Message) (int64, error) {
	f, err := os.CreateTemp("", "dataexport-*.zip")
	if err != nil {
		return 0, fmt.Errorf("dataexport: spool file: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if err := h.stream(ctx, m.Sub, &limitedWriter{w: f, left: h.maxPart}); err != nil {
		return 0, fmt.Errorf("dataexport: building part for %s: %w", m.RequestID, err)
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, fmt.Errorf("dataexport: spool size: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("dataexport: spool rewind: %w", err)
	}
	if err := h.putFile(ctx, m.PutURL, f, size); err != nil {
		return 0, err
	}
	return size, nil
}

// limitedWriter fails once more than left bytes are written.
type limitedWriter struct {
	w    io.Writer
	left int64
}

var errPartTooLarge = errors.New("dataexport: part exceeds the size the handler will spool")

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, errPartTooLarge
	}
	n, err := l.w.Write(p)
	l.left -= int64(n)
	return n, err
}

func (h *Handler) put(ctx context.Context, url string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("dataexport: put request: %w", err)
	}
	return h.send(req)
}

// putFile uploads size bytes from f. The length is set explicitly because
// net/http only infers it for in-memory readers, and without it the PUT would be
// chunked, which a presigned S3 PUT refuses.
func (h *Handler) putFile(ctx context.Context, url string, f *os.File, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, io.NewSectionReader(f, 0, size))
	if err != nil {
		return fmt.Errorf("dataexport: put request: %w", err)
	}
	req.ContentLength = size
	return h.send(req)
}

func (h *Handler) send(req *http.Request) error {
	req.Header.Set("Content-Type", "application/zip") // pinned by the presigned signature
	resp, err := h.http.Do(req)
	if err != nil {
		// err can quote the URL, whose query is a credential: keep only the cause.
		return fmt.Errorf("dataexport: upload failed: %w", errNoURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("dataexport: upload rejected: status %d: %s", resp.StatusCode, msg)
	}
	return nil
}
