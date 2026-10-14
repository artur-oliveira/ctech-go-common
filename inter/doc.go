// Package inter is Banco Inter's implementation of byok.Provider (PJ APIs:
// Pix, Banking, Token). Credentials are passed per call; the package reads no
// SSM, no environment and holds no token cache (callers own the token
// lifecycle). Endpoint shapes: billing's parecer 2026-10-11 §§ 2, 3, 6, 7.
//
// WithLogger turns on a raw trace of every Inter response (status, duration,
// body up to 8 KiB), with the integration's client_secret, the bearer and a token
// response's access_token redacted. Off by default; errors still carry only
// Inter's problem title, never its detail.
package inter
