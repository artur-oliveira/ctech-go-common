// Package pushsvc is the product-independent half of CTech Web Push: the subscription store, the subscribe routes, the
// notification payload, the sender core that runs in a Lambda, and the reminder store the scheduled pushes of Poker and
// Billing use. It sits on the stateless webpush package and holds no business rules: each product decides who gets
// what and when. See README.md.
package pushsvc
