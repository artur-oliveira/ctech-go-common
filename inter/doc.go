// Package inter is Banco Inter's implementation of byok.Provider (PJ APIs:
// Pix, Banking, Token). Credentials are passed per call; the package reads no
// SSM, no environment and holds no token cache (callers own the token
// lifecycle). Endpoint shapes: billing's parecer 2026-10-11 §§ 2, 3, 6, 7.
package inter
