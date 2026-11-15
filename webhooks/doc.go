// Package webhooks is the family's outbound-webhook engine: signing, the
// registration challenge, an SSRF-guarded HTTP client, the retry policy and a
// Deliverer that works over a small Store each service implements on its own
// storage. Routing an event to endpoints (fan-out) stays in the service.
package webhooks
