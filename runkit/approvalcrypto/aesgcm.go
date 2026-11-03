// Package approvalcrypto encrypts host-owned approval checkpoints without
// knowing their plaintext schema or persistence backend.
package approvalcrypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// envelopeVersion 2 authenticates the envelope header (version, key id, and
// key fingerprint) together with the caller's associated data.
const envelopeVersion = 2

var (
	ErrInvalidKeyProvider = errors.New("invalid approval key provider")
	ErrInvalidKeyMaterial = errors.New("invalid approval key material")
	ErrInvalidEnvelope    = errors.New("invalid approval ciphertext envelope")
	// ErrKeyMaterialChanged reports that the key stored under the envelope's
	// key id is not the key that encrypted it, for example because a deleted
	// keychain item was regenerated. The checkpoint cannot be recovered.
	ErrKeyMaterialChanged = errors.New("approval key was replaced since the checkpoint was encrypted")
)

// KeyMaterial is a host-owned data encryption key. Bytes must contain exactly
// 32 random bytes for AES-256-GCM and must never be persisted with ciphertext.
type KeyMaterial struct {
	ID    string
	Bytes []byte
}

// KeyProvider returns the active key for new checkpoints and resolves keys
// named by older ciphertext envelopes during a key-rotation window.
type KeyProvider interface {
	Active(ctx context.Context) (KeyMaterial, error)
	Resolve(ctx context.Context, keyID string) (KeyMaterial, error)
}

// AESGCMCipher encrypts versioned checkpoint envelopes with AES-256-GCM.
type AESGCMCipher struct {
	keys KeyProvider
}

func NewAESGCMCipher(keys KeyProvider) (*AESGCMCipher, error) {
	if keys == nil {
		return nil, ErrInvalidKeyProvider
	}
	return &AESGCMCipher{keys: keys}, nil
}

// Encrypt protects plaintext and authenticates aad without storing aad itself.
func (c *AESGCMCipher) Encrypt(ctx context.Context, plaintext, aad []byte) ([]byte, error) {
	if c == nil || c.keys == nil {
		return nil, ErrInvalidKeyProvider
	}
	key, err := c.keys.Active(ctx)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate approval nonce: %w", err)
	}
	header := ciphertextEnvelope{Version: envelopeVersion, KeyID: key.ID, KeyFingerprint: keyFingerprint(key)}
	header.Nonce = nonce
	header.Ciphertext = aead.Seal(nil, nonce, plaintext, header.sealedAAD(aad))
	envelope, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("encode approval ciphertext envelope: %w", err)
	}
	return envelope, nil
}

// Decrypt validates the envelope and associated data before returning plaintext.
func (c *AESGCMCipher) Decrypt(ctx context.Context, encoded, aad []byte) ([]byte, error) {
	if c == nil || c.keys == nil {
		return nil, ErrInvalidKeyProvider
	}
	var envelope ciphertextEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode", ErrInvalidEnvelope)
	}
	if envelope.Version != envelopeVersion || strings.TrimSpace(envelope.KeyID) == "" || envelope.KeyFingerprint == "" ||
		len(envelope.Nonce) == 0 || len(envelope.Ciphertext) == 0 {
		return nil, ErrInvalidEnvelope
	}
	key, err := c.keys.Resolve(ctx, envelope.KeyID)
	if err != nil {
		return nil, err
	}
	if key.ID != envelope.KeyID {
		return nil, ErrInvalidKeyMaterial
	}
	if keyFingerprint(key) != envelope.KeyFingerprint {
		return nil, ErrKeyMaterialChanged
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(envelope.Nonce) != aead.NonceSize() {
		return nil, ErrInvalidEnvelope
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, envelope.sealedAAD(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: authentication", ErrInvalidEnvelope)
	}
	return plaintext, nil
}

type ciphertextEnvelope struct {
	Version        int    `json:"version"`
	KeyID          string `json:"key_id"`
	KeyFingerprint string `json:"key_fingerprint"`
	Nonce          []byte `json:"nonce"`
	Ciphertext     []byte `json:"ciphertext"`
}

// sealedAAD is the associated data GCM authenticates: the header fields,
// length-prefixed so no two headers encode alike, followed by the caller's aad.
func (e ciphertextEnvelope) sealedAAD(aad []byte) []byte {
	out := []byte("goagents/approvalcrypto")
	out = binary.AppendUvarint(out, uint64(e.Version))
	for _, field := range []string{e.KeyID, e.KeyFingerprint} {
		out = binary.AppendUvarint(out, uint64(len(field)))
		out = append(out, field...)
	}
	return append(out, aad...)
}

// keyFingerprint identifies key material without revealing it, so Decrypt can
// tell a replaced key from tampered ciphertext.
func keyFingerprint(key KeyMaterial) string {
	digest := sha256.Sum256(append([]byte("goagents/approvalcrypto key fingerprint\x00"), key.Bytes...))
	return hex.EncodeToString(digest[:8])
}

func newAEAD(key KeyMaterial) (cipher.AEAD, error) {
	if strings.TrimSpace(key.ID) == "" || len(key.Bytes) != 32 {
		return nil, ErrInvalidKeyMaterial
	}
	block, err := aes.NewCipher(key.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: AES setup", ErrInvalidKeyMaterial)
	}
	return cipher.NewGCM(block)
}
