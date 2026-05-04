// Package calltypes loads a confidential list of dispatch call types from an encrypted
// file. The plaintext is one call type per line. The ciphertext is committed to the repo;
// the decryption key is supplied at runtime via an environment variable.
//
// Format on disk: 12-byte AES-GCM nonce || ciphertext || 16-byte GCM auth tag.
// Cipher: AES-256-GCM. Key length: exactly 32 bytes, hex-encoded in the env.
//
// The encrypted file is ciphertext-indistinguishable: a fresh nonce per encryption means
// re-encrypting the same plaintext produces a different blob, so committing changes after
// editing the list looks like a real diff (no leaks via "no-op commit" inference).
package calltypes

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// KeyLength is the required size of the AES-256 key in bytes.
	KeyLength = 32
	// nonceLength is the GCM nonce size; standard library default.
	nonceLength = 12
)

var (
	ErrEmptyKey       = errors.New("call-types key is empty")
	ErrInvalidKey     = errors.New("call-types key is not a valid hex-encoded 32-byte value")
	ErrBlobTooShort   = errors.New("encrypted blob is shorter than the GCM nonce")
	ErrDecryptFailed  = errors.New("failed to decrypt call-types file (wrong key or tampered ciphertext)")
	ErrEmptyCallTypes = errors.New("decrypted call-types file is empty")
)

// ParseKey decodes a hex-encoded 32-byte key. Returns ErrInvalidKey on length / encoding
// problems so callers can wrap with friendlier messages.
func ParseKey(hexKey string) ([]byte, error) {
	if hexKey == "" {
		return nil, ErrEmptyKey
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidKey, err.Error())
	}
	if len(key) != KeyLength {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidKey, len(key), KeyLength)
	}
	return key, nil
}

// GenerateKey returns a freshly-generated random 32-byte key, hex-encoded. Use this once
// per deployment and store the result in your secret manager / env var.
func GenerateKey() (string, error) {
	key := make([]byte, KeyLength)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("failed to generate key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// Encrypt seals plaintext with the supplied key. Output layout: nonce || ciphertext+tag.
// Used by cmd/encrypt-calltypes to produce the committed file; not called at runtime.
func Encrypt(plaintext, key []byte) ([]byte, error) {
	if len(key) != KeyLength {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidKey, len(key), KeyLength)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher.NewGCM: %w", err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("rand.Read nonce: %w", err)
	}

	// Seal appends ciphertext+tag onto the dst slice (here: the nonce), giving us the
	// nonce || ciphertext layout in a single allocation.
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens an encrypted blob with the supplied key. Inverse of Encrypt.
func Decrypt(blob, key []byte) ([]byte, error) {
	if len(key) != KeyLength {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidKey, len(key), KeyLength)
	}
	if len(blob) < nonceLength {
		return nil, ErrBlobTooShort
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher.NewGCM: %w", err)
	}
	nonce, ciphertext := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Don't surface the internal GCM error — it can leak whether the failure was
		// "wrong key" vs "tampered tag", and we want callers to see a single mode.
		return nil, ErrDecryptFailed
	}
	return plaintext, nil
}

// Load reads the file at path, decrypts with hexKey, and returns one entry per non-empty
// line. Trims whitespace, ignores blank lines, deduplicates while preserving order.
func Load(path, hexKey string) ([]string, error) {
	key, err := ParseKey(hexKey)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encrypted call-types file %q: %w", path, err)
	}
	plaintext, err := Decrypt(blob, key)
	if err != nil {
		return nil, err
	}
	return parseLines(plaintext), nil
}

func parseLines(plaintext []byte) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, raw := range strings.Split(string(plaintext), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return out
}
