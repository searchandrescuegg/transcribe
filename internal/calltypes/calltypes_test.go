package calltypes

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip(t *testing.T) {
	hexKey, err := GenerateKey()
	require.NoError(t, err)
	key, err := ParseKey(hexKey)
	require.NoError(t, err)

	plaintext := []byte("Aid Emergency\nMVC\nRescue - Trail\n")
	blob, err := Encrypt(plaintext, key)
	require.NoError(t, err)

	got, err := Decrypt(blob, key)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestEncryptProducesDifferentBlobsEachTime(t *testing.T) {
	// Same plaintext, same key, two encryptions — random nonce means different ciphertext.
	// Important so committed-file diffs don't reveal "no actual change".
	hexKey, err := GenerateKey()
	require.NoError(t, err)
	key, _ := ParseKey(hexKey)
	plaintext := []byte("same input")

	a, err := Encrypt(plaintext, key)
	require.NoError(t, err)
	b, err := Encrypt(plaintext, key)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "fresh nonce per encryption should yield distinct ciphertexts")
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	hexA, _ := GenerateKey()
	hexB, _ := GenerateKey()
	keyA, _ := ParseKey(hexA)
	keyB, _ := ParseKey(hexB)

	blob, err := Encrypt([]byte("secret"), keyA)
	require.NoError(t, err)

	_, err = Decrypt(blob, keyB)
	assert.ErrorIs(t, err, ErrDecryptFailed)
}

func TestDecryptDetectsTampering(t *testing.T) {
	hexKey, _ := GenerateKey()
	key, _ := ParseKey(hexKey)
	blob, err := Encrypt([]byte("secret"), key)
	require.NoError(t, err)

	// Flip one bit in the ciphertext body (after the nonce).
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01

	_, err = Decrypt(tampered, key)
	assert.ErrorIs(t, err, ErrDecryptFailed, "GCM tag must reject any modification")
}

func TestDecryptShortBlob(t *testing.T) {
	hexKey, _ := GenerateKey()
	key, _ := ParseKey(hexKey)
	_, err := Decrypt([]byte("too short"), key)
	assert.ErrorIs(t, err, ErrBlobTooShort)
}

func TestParseKey_InvalidInputs(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"odd length": "deadbee",
		"non-hex":    "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"too short":  hex.EncodeToString(make([]byte, 16)), // 16 bytes
		"too long":   hex.EncodeToString(make([]byte, 64)), // 64 bytes
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseKey(in)
			assert.Error(t, err, "input %q should be rejected", in)
		})
	}
}

func TestParseLinesDeduplicatesAndTrims(t *testing.T) {
	in := []byte("  Aid Emergency  \nMVC\n\n  MVC  \nRescue - Trail\n")
	got := parseLines(in)
	assert.Equal(t, []string{"Aid Emergency", "MVC", "Rescue - Trail"}, got)
}

func TestLoad_HappyPath(t *testing.T) {
	hexKey, _ := GenerateKey()
	key, _ := ParseKey(hexKey)
	blob, err := Encrypt([]byte("Aid Emergency\nMVC\nRescue - Trail\n"), key)
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "call_types.enc")
	require.NoError(t, os.WriteFile(path, blob, 0o600))

	got, err := Load(path, hexKey)
	require.NoError(t, err)
	assert.Equal(t, []string{"Aid Emergency", "MVC", "Rescue - Trail"}, got)
}

func TestLoad_MissingFile(t *testing.T) {
	hexKey, _ := GenerateKey()
	_, err := Load("/nonexistent/path/call_types.enc", hexKey)
	require.Error(t, err)
	// Sanity-check the message points at the path so operators can debug.
	assert.True(t, strings.Contains(err.Error(), "call_types.enc"))
}
