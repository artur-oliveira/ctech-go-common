# observability/fiber

Fiber v3 glue for the observability contract: `RequestID` (correlation header) and `AccessLog` (below).

## AccessLog

```go
app.Use(fiberobs.RequestID())
app.Use(fiberobs.AccessLog()) // or AccessLogConfig{Output, Skip, Headers}
```

One JSON object per request: `msg` (`http_request`), `time`, `status`, `latency`, `method`, `path`, `request_id`,
optional `headers`.

- Skips paths ending in `/health` and `/health-check` (probes outnumber traffic). Override with `Skip`.
- `Authorization`, `Cookie`, `Set-Cookie`, `Proxy-Authorization`, `X-API-Key` are logged as `[REDACTED]` if listed
  in `Headers`; no header is logged unless listed. The query string is never logged.
- Extracted from ctech-billing's `accessLog()` (`internal/app/app.go`, `accesslog_test.go`). It fixes the two bugs
  billing documented in wallet/poker/dfe: `${request-id}` is not a Fiber tag so `request_id` was always empty
  (here it comes from the context set by `RequestID`, falling back to the response header, any mount order), and
  ANSI colour codes corrupted the JSON on stdout (nothing is coloured). A third fix: an error returned by a handler
  is logged with its real status (the app's `ErrorHandler` runs first) instead of 200.
- Each request is a single `Write` to `Output` (default stdout), serialised by a mutex.

## Migration

| Product | Replace |
|---|---|
| billing | `accessLog()` and `logger.New(accessLog())` in `internal/app/app.go` (keep its `RequestID` if not yet on this package's) |
| wallet | `logger.New(logger.Config{...})` in `internal/app/app.go` |
| poker, dfe | the equivalent `logger.New(...)` block in `internal/app/app.go` |

Format changes consumers must check: `msg` is new, `latency` is a Go duration string as before, and the timestamp
is RFC3339Nano UTC. Verify CloudWatch metric filters before switching.
