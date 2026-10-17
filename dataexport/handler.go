package dataexport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// PartFunc builds the service's part for sub: a zip with JSON (and CSV) files
// and a README (spec E3/E4). It must be idempotent: a redelivery builds again
// and overwrites the same object.
// ponytail: the part is held in memory because a presigned PUT needs a
// Content-Length. Parts are one person's data; stream through a multipart
// upload if one ever outgrows that.
type PartFunc func(ctx context.Context, sub string) ([]byte, error)

// Acker reports the upload to ctech-account.
type Acker interface {
	Send(ctx context.Context, a Ack) error
}

type Handler struct {
	service string
	part    PartFunc
	http    *http.Client
	acks    Acker
	now     func() time.Time
}

func NewHandler(service string, part PartFunc, httpClient *http.Client, acks Acker) *Handler {
	return &Handler{service: service, part: part, http: httpClient, acks: acks, now: time.Now}
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
	data, err := h.part(ctx, m.Sub)
	if err != nil {
		return true, fmt.Errorf("dataexport: building part for %s: %w", m.RequestID, err)
	}
	if err := h.put(ctx, m.PutURL, data); err != nil {
		return true, err
	}
	return true, h.acks.Send(ctx, Ack{RequestID: m.RequestID, Service: h.service, Result: ResultDone, Size: int64(len(data)), At: h.now().UTC()})
}

func (h *Handler) put(ctx context.Context, url string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("dataexport: put request: %w", err)
	}
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
