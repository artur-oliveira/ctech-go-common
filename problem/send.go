package problem

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"gopkg.aoctech.app/api-commons/observability"
	fiberobs "gopkg.aoctech.app/api-commons/observability/fiber"
)

// ContentType is the RFC 7807 media type.
const ContentType = "application/problem+json"

const (
	detailInternal = "an unexpected error occurred"
	detailTimeout  = "the request took too long; try again"
	// fallbackBody is written when a problem cannot be marshalled (e.g. a
	// WithExtra value that is not JSON-encodable): the client still gets a
	// problem+json 500 instead of Fiber's plain-text default.
	fallbackBody = `{"type":"` + TypeInternalServer + `","title":"Internal Server Error","status":500,"detail":"` + detailInternal + `"}`
)

// MarshalBody encodes the problem including WithExtra members. Typed members
// win over an extra with the same key.
func (p *Problem) MarshalBody() ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil || len(p.extras) == 0 {
		return b, err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range p.extras {
		if _, taken := m[k]; taken {
			continue
		}
		if m[k], err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	return json.Marshal(m)
}

// Send writes the problem as application/problem+json, sets Retry-After when
// RetryAfterSeconds > 0 (body and header stay in sync) and logs the internal
// cause with the request id; the cause never reaches the body. A status
// outside 400-599 is sent as 500: a Problem is an error, never a success.
func (p *Problem) Send(c fiber.Ctx) error {
	status := p.Status
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusInternalServerError
	}
	fiberobs.LogHTTPError(c, status, p.Type, p.cause)
	c.Status(status)
	c.Set(fiber.HeaderContentType, ContentType)
	if p.RetryAfterSeconds > 0 {
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(p.RetryAfterSeconds))
	}
	body, err := p.MarshalBody()
	if err != nil {
		observability.Error(c.Context(), "problem response marshal failed", err,
			"status", status, "problem_type", p.Type, "method", c.Method(), "path", c.Path())
		c.Status(http.StatusInternalServerError)
		return c.SendString(fallbackBody)
	}
	return c.Send(body)
}

// FromError maps any error to the problem to send. nil maps to nil.
//
//   - *Problem (anywhere in the chain): itself.
//   - *fiber.Error: 504 and 503 keep their meaning; other 4xx keep their
//     status with the fiber message as detail; 5xx otherwise become a generic
//     500 (a server-side message is not for clients).
//   - context.DeadlineExceeded: 504.
//   - anything else: a generic 500 whose detail says nothing about the error.
//
// The original error is attached as the cause (logged, never serialized).
func FromError(err error) *Problem {
	if err == nil {
		return nil
	}
	if p, ok := errors.AsType[*Problem](err); ok {
		return p
	}
	if fe, ok := errors.AsType[*fiber.Error](err); ok {
		switch {
		case fe.Code == http.StatusGatewayTimeout:
			return GatewayTimeout(detailTimeout).WithCause(err)
		case fe.Code == http.StatusServiceUnavailable:
			return ServiceUnavailable(fe.Message).WithCause(err)
		case fe.Code >= http.StatusBadRequest && fe.Code < http.StatusInternalServerError:
			return fromClientStatus(fe.Code, fe.Message).WithCause(err)
		}
		return InternalServer(detailInternal).WithCause(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return GatewayTimeout(detailTimeout).WithCause(err)
	}
	return InternalServer(detailInternal).WithCause(err)
}

func fromClientStatus(code int, msg string) *Problem {
	switch code {
	case http.StatusBadRequest:
		return BadRequest(msg)
	case http.StatusUnauthorized:
		return Unauthorized(msg)
	case http.StatusForbidden:
		return Forbidden(msg)
	case http.StatusNotFound:
		return NotFound(msg)
	case http.StatusConflict:
		return Conflict(msg)
	case http.StatusUnprocessableEntity:
		return UnprocessableEntity(msg)
	case http.StatusTooManyRequests:
		return TooManyRequests(msg)
	}
	title := http.StatusText(code)
	if title == "" {
		title = "Request Error"
	}
	return New(code, TypeHTTPError, title, msg)
}

// ErrorHandler is a fiber.ErrorHandler: set it as fiber.Config.ErrorHandler so
// every unhandled error becomes an RFC 7807 body. It fails closed: an error it
// does not recognise is a generic 500, never its own text.
func ErrorHandler(c fiber.Ctx, err error) error {
	return FromError(err).Send(c)
}
