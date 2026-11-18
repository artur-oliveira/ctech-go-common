package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

const (
	saltLen        = 16
	headerLen      = saltLen + 4 + 1 + pubKeyLen // salt, rs, idlen, keyid (86)
	lastRecordFlag = 0x02                        // RFC 8188: delimiter of the final record
)

// Encrypt builds the aes128gcm body of a push message (RFC 8291 over RFC 8188):
// one record, a fresh ephemeral key and a random salt per message.
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: ephemeral key: %w", err)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("webpush: salt: %w", err)
	}
	return encrypt(sub, plaintext, asPriv, salt)
}

// encrypt is Encrypt with the two random inputs injected, so the RFC 8291
// vector can be reproduced byte for byte.
func encrypt(sub Subscription, plaintext []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrPayloadTooLarge, len(plaintext), MaxPlaintext)
	}
	if len(sub.Auth) != authLen {
		return nil, fmt.Errorf("%w: auth must be %d bytes", ErrInvalidSubscription, authLen)
	}
	ua, err := ecdh.P256().NewPublicKey(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("%w: p256dh: %v", ErrInvalidSubscription, err)
	}
	secret, err := asPriv.ECDH(ua)
	if err != nil {
		return nil, fmt.Errorf("webpush: ecdh: %w", err)
	}
	asPub := asPriv.PublicKey().Bytes()

	cek, nonce, err := deriveKeys(secret, sub.Auth, sub.P256dh, asPub, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record := make([]byte, 0, len(plaintext)+1)
	record = append(append(record, plaintext...), lastRecordFlag)

	body := make([]byte, 0, headerLen+len(record)+gcm.Overhead())
	body = append(body, salt...)
	body = binary.BigEndian.AppendUint32(body, RecordSize)
	body = append(body, byte(len(asPub)))
	body = append(body, asPub...)
	return gcm.Seal(body, nonce, record, nil), nil
}

// deriveKeys is RFC 8291 section 3.4 and RFC 8188 section 2.2: the content
// encryption key and nonce from the ECDH secret, the auth secret and the salt.
func deriveKeys(ecdhSecret, auth, uaPub, asPub, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, ecdhSecret, auth)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	if nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12); err != nil {
		return nil, nil, err
	}
	return cek, nonce, nil
}
