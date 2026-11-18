// Package webpush sends Web Push messages (RFC 8030, 8291 and 8292) using only the
// standard library: subscription validation, aes128gcm encryption, VAPID keys and
// headers, and a Sender that classifies the push service's answer. It holds no
// state; subscriptions, preferences and fan-out stay in the service. See README.md.
package webpush
