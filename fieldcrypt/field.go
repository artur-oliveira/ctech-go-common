// Package fieldcrypt encrypts individual field values before they are stored.
// It is the family's one implementation (extracted from ctech-billing); do not
// copy it into a service.
//
// DynamoDB's own server-side encryption protects the disk. It does not protect
// the value from anything holding read access to the table — a support tool, a
// migration script, an export, a role that accumulated `dynamodb:Query` for some
// other reason. For a CPF that difference is the whole of the control: the
// record a service promises as "encrypted at rest by the repository layer" is only
// true if something like this runs in that layer.
//
// Scope is deliberately narrow: one AEAD and values that are read back by id and
// never searched. Anything that needed to be *queried* by its encrypted value
// would need deterministic encryption, which leaks equality and is a different
// decision — no service has such a field, and this package should not grow one
// quietly.
//
// Several keys may be held at once, which is what makes rotation possible: the
// highest generation writes, every generation reads. See NewSealer.
package fieldcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A stored value is `v<generation>.<base64>`.
//
// The generation is what makes rotation something other than guesswork: without
// it the only way to know whether a string is v1 or v2 ciphertext is to try a
// key and see whether the tag verifies, which is indistinguishable from
// tampering. With it a second key is additive — new writes stamp v2, old reads
// still resolve v1, and nothing has to be re-encrypted before the new key is in
// use.

// ErrNotSealed reports a stored value that this package did not produce.
//
// It is an error rather than a pass-through. Returning the raw value when the
// prefix is missing would mean a deployment that silently stopped encrypting
// reads exactly like one that never started, and the failure would be
// discovered by whoever exports the table.
var ErrNotSealed = errors.New("value is not sealed")

// ErrNoKey reports a Sealer built without a usable key.
var ErrNoKey = errors.New("field encryption key is missing or invalid")

// ErrUnknownGeneration reports a value sealed by a key this deployment does not
// hold. It is its own error because the remedy is specific and nothing else
// says it: the old key was dropped from the configuration too early, and it has
// to go back before that row can be read.
var ErrUnknownGeneration = errors.New("value was sealed by a key this deployment does not hold")

// Sealer encrypts and decrypts one field at a time.
//
// A construction failure is carried rather than returned, so a repository
// constructor stays a constructor. It is safe because the key is validated at
// startup by the service's config loading: the only way to hold a broken Sealer is to build a
// Config by hand, which is a test, where failing loudly on first use is the
// behaviour worth having.
type Sealer struct {
	// keys is every generation this deployment can read.
	keys map[int]cipher.AEAD
	// write is the generation new values are sealed with: the highest one
	// configured. Writing with the newest key and reading with all of them is
	// the whole of the rotation procedure — add the new key, deploy, and old
	// rows keep opening until something rewrites them.
	write int
	err   error
}

// NewSealer builds a Sealer from one or more 32-byte keys given as base64 or
// hex.
//
// Both encodings are accepted because ctech-account's SECRET_ENC_KEY accepts
// both, and an operator who has generated one of these before should not have to
// discover that this service is fussier.
//
// The configured value is a comma-separated list of `generation:key` entries,
// and a bare key with no generation is generation 1 — which is what every
// deployment written before rotation existed already holds, so nothing has to
// change to keep working. Rotating is two deploys and no migration:
//
//	FIELD_ENCRYPTION_KEY="2:<new>,1:<old>"   # writes v2, still reads v1
//	FIELD_ENCRYPTION_KEY="2:<new>"           # once nothing v1 is left
//
// Dropping the old key is the irreversible step, and it is deliberately the
// operator's own decision rather than something this package times: only they
// know whether any v1 row survives. A value whose generation is gone opens with
// ErrUnknownGeneration rather than "did not verify", because those two have
// completely different remedies.
func NewSealer(keys string) *Sealer {
	parsed, err := decodeKeys(keys)
	if err != nil {
		return &Sealer{err: err}
	}
	s := &Sealer{keys: make(map[int]cipher.AEAD, len(parsed))}
	for gen, raw := range parsed {
		block, err := aes.NewCipher(raw)
		if err != nil {
			return &Sealer{err: fmt.Errorf("%w: %s", ErrNoKey, err)}
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return &Sealer{err: fmt.Errorf("%w: %s", ErrNoKey, err)}
		}
		s.keys[gen] = aead
		if gen > s.write {
			s.write = gen
		}
	}
	return s
}

// WriteGeneration is the generation new values are sealed with. Exported for
// the operator-facing surfaces that report which key is in use; nothing in the
// data path branches on it.
func (s *Sealer) WriteGeneration() int { return s.write }

// Enabled reports whether this Sealer can be used at all.
func (s *Sealer) Enabled() bool { return s != nil && s.err == nil }

// Seal encrypts a value. Empty in, empty out — an absent tax id is absent, not
// a ciphertext of nothing, which would make every unset field distinguishable
// and identical.
func (s *Sealer) Seal(plaintext string) (string, error) { return s.SealWith(plaintext, nil) }

// SealWith is Seal bound to associated data: the value only opens with the same
// bytes. Callers pass something that names where the value lives (table, field
// and the row's key, e.g. "webhooks/secret/"+pk+"/"+sk), so a ciphertext copied
// to another row or field fails to verify instead of opening there. The data is
// not stored; the caller rebuilds it on read. nil is what Seal uses.
func (s *Sealer) SealWith(plaintext string, aad []byte) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if s == nil || s.err != nil {
		return "", s.keyErr()
	}
	aead := s.keys[s.write]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), aad)
	return "v" + strconv.Itoa(s.write) + "." + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal.
func (s *Sealer) Open(stored string) (string, error) { return s.OpenWith(stored, nil) }

// OpenWith reverses SealWith; aad must be the bytes the value was sealed with.
func (s *Sealer) OpenWith(stored string, aad []byte) (string, error) {
	if stored == "" {
		return "", nil
	}
	if s == nil || s.err != nil {
		return "", s.keyErr()
	}
	gen, body, ok := splitGeneration(stored)
	if !ok {
		return "", ErrNotSealed
	}
	aead, ok := s.keys[gen]
	if !ok {
		return "", fmt.Errorf("%w: v%d", ErrUnknownGeneration, gen)
	}
	raw, err := base64.RawStdEncoding.DecodeString(body)
	if err != nil {
		return "", fmt.Errorf("decoding sealed value: %w", err)
	}
	if len(raw) < aead.NonceSize() {
		return "", errors.New("sealed value is too short")
	}
	nonce, ct := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		// Deliberately not wrapped with the underlying message. A GCM failure is
		// either the wrong key, other associated data or a tampered value, and neither is something to
		// describe to a caller in detail.
		return "", errors.New("sealed value did not verify")
	}
	return string(plaintext), nil
}

func (s *Sealer) keyErr() error {
	if s == nil {
		return ErrNoKey
	}
	return s.err
}

// splitGeneration parses `v<n>.<body>`.
func splitGeneration(stored string) (gen int, body string, ok bool) {
	rest, ok := strings.CutPrefix(stored, "v")
	if !ok {
		return 0, "", false
	}
	digits, body, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, "", false
	}
	gen, err := strconv.Atoi(digits)
	if err != nil || gen < 1 {
		return 0, "", false
	}
	return gen, body, true
}

// decodeKeys parses the configured list into one raw key per generation.
func decodeKeys(keys string) (map[int][]byte, error) {
	out := map[int][]byte{}
	for entry := range strings.SplitSeq(keys, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		gen := 1
		if digits, key, found := strings.Cut(entry, ":"); found {
			n, err := strconv.Atoi(strings.TrimSpace(digits))
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%w: generation must be a positive integer", ErrNoKey)
			}
			gen, entry = n, key
		}
		raw, err := decodeKey(entry)
		if err != nil {
			return nil, err
		}
		// Two keys claiming one generation is a configuration mistake with no
		// safe reading: one of them wrote rows the other cannot open.
		if _, dup := out[gen]; dup {
			return nil, fmt.Errorf("%w: generation %d is configured twice", ErrNoKey, gen)
		}
		out[gen] = raw
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrNoKey)
	}
	return out, nil
}

func decodeKey(key string) ([]byte, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("%w: empty", ErrNoKey)
	}
	// No dev fallback, and that is a deliberate departure from ctech-account's
	// crypto package, which encrypts with a constant when the variable is unset.
	// A default key is a key in the repository: everything encrypted with it is
	// encrypted with something published, and the deployment that forgets to set
	// the real one looks identical to the one that did not.
	if b, err := base64.StdEncoding.DecodeString(key); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := hex.DecodeString(key); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("%w: must decode to exactly 32 bytes as base64 or hex", ErrNoKey)
}

// IsSealed reports whether s carries the sealer's own `v<n>.` prefix (it says
// nothing about whether it opens). Used to prove a cached value is not plain.
func IsSealed(s string) bool { _, _, ok := splitGeneration(s); return ok }
