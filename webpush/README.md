# webpush

Web Push for Go services, with the standard library only: RFC 8030 (protocol), RFC 8291 (message
encryption, `aes128gcm`) and RFC 8292 (VAPID). No third-party push library. Stateless: it holds no
subscriptions and talks to no database; the service stores subscriptions and decides who gets what.

Design: `ctech-dfe/docs/specs/2026-11-17-web-push.md`.

## Use

```go
// Once, at startup: one client and one Sender per service. keys are every VAPID key still in use.
key, _ := webpush.ParseKey(pemFromSecretStore)
sender := webpush.NewSender(webpush.NewClient(), "mailto:ops@example.com", key)

// At subscribe time (the browser's PushSubscription.toJSON values):
sub, err := webpush.ParseSubscription(endpoint, p256dh, auth) // store sub and key.KID together

// At send time:
res, err := sender.Send(ctx, sub, kid, webpush.Message{
    Payload:   payload,               // <= webpush.MaxPlaintext; products set a smaller ceiling
    TTL:       24 * time.Hour,        // required
    Urgency:   webpush.UrgencyNormal,
    Topic:     "document.rejected",   // optional: replaces a pending message with the same topic
    DedupeKey: eventID,               // optional: lets the client retry a transient failure once
})
switch res.Outcome {
case webpush.Gone, webpush.Unauthorized: // delete the subscription
case webpush.PayloadTooLarge:            // our bug: alert
case webpush.Throttled:                  // dropped; best effort
case webpush.Transient:                  // err says why (timeout, open circuit)
}
```

Generate a key pair for the secret store with `webpush.GenerateKey()` (PEM PKCS#8 plus the `Key`);
give browsers `key.PublicBase64URL()` as `applicationServerKey`.

## Behaviour

| Answer | `Outcome` | What the caller does |
|---|---|---|
| 200, 201, 202 | `Delivered` | nothing |
| 404, 410 | `Gone` | delete the subscription |
| 413 | `PayloadTooLarge` | bug on our side, no retry |
| 401, 403 | `Unauthorized` | key does not match the subscription: delete it |
| 429 | `Throttled` | a `Retry-After` up to 2 s is waited out once; longer is dropped (`Result.RetryAfter`) |
| other 4xx, 3xx | `Rejected` | log, no retry |
| 5xx, timeout, network, open circuit | `Transient` | `err` is set; the client retries once when `DedupeKey` is given |

`Send` returns an error only when no answer was obtained or the message would be refused before the
network: `ErrInvalidMessage`, `ErrPayloadTooLarge`, `ErrUnknownKey`, `ErrInvalidSubscription`, or the
transport error (wrapping `resilient.ErrCircuitOpen` when the host's circuit is open).

- **SSRF:** `NewClient` uses the transport of `webhooks.NewGuardedClient` (dial refuses non-public
  addresses, no redirect, no proxy), and `ParseSubscription` only accepts `AllowedHosts` over https on
  the default port. The endpoint is a secret (it identifies the browser): never log it.
- **Secrets stay out of errors and logs:** the endpoint path is a per-subscription token, so no error,
  `Result` or printed value of this package carries the endpoint, `p256dh`, `auth`, the encrypted body or
  the VAPID JWT. A transport failure is returned as `webpush: send to <host>: <cause>` (the `*url.Error`
  that `http.Client.Do` builds, with the full URL, is unwrapped; `errors.Is` and `errors.As` still reach the
  cause). `Subscription`, `Key` and `Message` redact in `%v`, `%+v`, `%#v`, `%s` and `slog` (host, `KID` and
  payload size only). Callers must follow the rule: log `sub` or `sub.String()`, never `sub.Endpoint`, and
  never an `*url.Error` from elsewhere. `redact_test.go` fails if the endpoint, a key, `vapid t=` or a JWT
  shows up in any error path (network, timeout, open circuit, refused redirect, guarded dial).
- **Retry:** the underlying `resilient` client retries a POST only with an `Idempotency-Key`; `Send`
  derives it from `DedupeKey` and the endpoint (hashed, the endpoint itself never travels as a key).
  A duplicate is absorbed on the device by the notification `tag`. One circuit per push-service host.
- **VAPID:** a token per send, `exp` one hour ahead, `aud` the endpoint origin, signature as 64 bytes
  `r||s`. `Key.KID` is a stable 8-character id of the public key, stored with each subscription so a
  rotation can keep the previous key available until clients resubscribe.
- **Payload:** `MaxPlaintext` is 3993 bytes (RFC 8291 section 4: a 4096-byte body minus the 86-byte
  header and 17 bytes of delimiter and tag). Keep real payloads small and free of fiscal or personal
  data: push services see the encrypted body, the lock screen shows the title.

## Tests

- `testdata/rfc8291.json`: the values of RFC 8291 section 5 and appendix A, extracted from the RFC
  text by script. `TestEncryptMatchesRFC8291Vector` reproduces the RFC body byte for byte (and the
  ECDH secret, CEK and nonce).
- `testdata/notification-v1.json`: the golden notification payload of contract v1
  (Declarative Web Push shape). `push-client` copies it and tests the service worker against it; a
  change is a contract change (bump `data.v`).
- Round trips with a user agent decryptor at 0, 1, 1024 and `MaxPlaintext` bytes, VAPID signature
  verification (raw `r||s`, 600 signatures to hit short integers), `FuzzParseSubscription`, and the
  `Sender` against `httptest` for every row above, Retry-After, retry with and without `DedupeKey`,
  breaker, timeout and the guarded client refusing loopback.

`go test ./webpush -race`, `staticcheck` and `govulncheck ./webpush` are clean.
