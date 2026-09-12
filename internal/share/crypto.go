package share

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeySize is the AES-256 key length and IVSize the GCM nonce length.
const (
	KeySize = 32
	IVSize  = 12
)

// ErrTampered reports a snapshot that failed authentication: the wrong key,
// or a payload that was modified after sealing. GCM makes the two
// indistinguishable by design, and neither ever yields partial plaintext.
var ErrTampered = errors.New("share: snapshot failed authentication (wrong key or modified blob)")

// NewKey returns a fresh AES-256 key.
func NewKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("share: key: %w", err)
	}
	return key, nil
}

// Seal gzips plaintext and seals it under a fresh AES-256-GCM key. The blob
// is [12B IV][ciphertext+tag]; the key is what the link fragment carries.
func Seal(plaintext []byte) (blob, key []byte, err error) {
	key, err = NewKey()
	if err != nil {
		return nil, nil, err
	}
	blob, err = SealWith(plaintext, key)
	if err != nil {
		return nil, nil, err
	}
	return blob, key, nil
}

// SealWith seals under a caller-supplied key.
func SealWith(plaintext, key []byte) ([]byte, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plaintext); err != nil {
		return nil, fmt.Errorf("share: gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("share: gzip: %w", err)
	}
	iv := make([]byte, IVSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, fmt.Errorf("share: iv: %w", err)
	}
	// Seal appends the tag to dst; dst starts as the IV so the result is
	// exactly [IV][ciphertext+tag].
	return gcm.Seal(iv, iv, buf.Bytes(), nil), nil
}

// Open reverses Seal: authenticate + decrypt, then gunzip.
func Open(blob, key []byte) ([]byte, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < IVSize+gcm.Overhead() {
		return nil, ErrTampered
	}
	plain, err := gcm.Open(nil, blob[:IVSize], blob[IVSize:], nil)
	if err != nil {
		return nil, ErrTampered
	}
	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("share: gunzip: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("share: gunzip: %w", err)
	}
	return out, nil
}

// gcmFor builds the AEAD for a key, validating its length.
func gcmFor(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("share: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("share: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("share: gcm: %w", err)
	}
	return gcm, nil
}

// EncodeKey renders a key for a link fragment (base64url, unpadded).
func EncodeKey(key []byte) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

// DecodeKey parses a link-fragment key (padded and unpadded base64url alike).
func DecodeKey(s string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, fmt.Errorf("share: key: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("share: key must be %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}
