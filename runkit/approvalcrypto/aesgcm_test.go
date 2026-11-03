package approvalcrypto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestAESGCMCipherRoundTripsBoundCheckpoint(t *testing.T) {
	provider := testKeyProvider{activeID: "local-v1", keys: map[string][]byte{
		"local-v1": bytes.Repeat([]byte{1}, 32),
	}}
	cipher, err := NewAESGCMCipher(provider)
	if err != nil {
		t.Fatalf("NewAESGCMCipher: %v", err)
	}
	plaintext := []byte(`{"pending_calls":["write"]}`)
	aad := []byte(`{"checkpoint_id":"checkpoint-1","tenant_id":"tenant-1","definition_hash":"agent-v1"}`)
	ciphertext, err := cipher.Encrypt(context.Background(), plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext must not equal plaintext")
	}
	decoded, err := cipher.Decrypt(context.Background(), ciphertext, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(decoded, plaintext) {
		t.Fatalf("plaintext = %q, want %q", decoded, plaintext)
	}
}

func TestAESGCMCipherRejectsTamperingAndWrongCheckpointBinding(t *testing.T) {
	provider := testKeyProvider{activeID: "local-v1", keys: map[string][]byte{
		"local-v1": bytes.Repeat([]byte{2}, 32),
	}}
	cipher, err := NewAESGCMCipher(provider)
	if err != nil {
		t.Fatalf("NewAESGCMCipher: %v", err)
	}
	aad := []byte(`{"checkpoint_id":"checkpoint-1","tenant_id":"tenant-1","definition_hash":"agent-v1"}`)
	ciphertext, err := cipher.Encrypt(context.Background(), []byte("secret"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 1
	if _, err := cipher.Decrypt(context.Background(), tampered, aad); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
	if _, err := cipher.Decrypt(context.Background(), ciphertext, []byte(`{"checkpoint_id":"checkpoint-1","tenant_id":"other-tenant","definition_hash":"agent-v1"}`)); err == nil {
		t.Fatal("ciphertext decrypted under another tenant binding")
	}
}

type testKeyProvider struct {
	activeID string
	keys     map[string][]byte
}

func (p testKeyProvider) Active(context.Context) (KeyMaterial, error) {
	key, ok := p.keys[p.activeID]
	if !ok {
		return KeyMaterial{}, fmt.Errorf("missing active key")
	}
	return KeyMaterial{ID: p.activeID, Bytes: append([]byte(nil), key...)}, nil
}

func (p testKeyProvider) Resolve(_ context.Context, keyID string) (KeyMaterial, error) {
	key, ok := p.keys[keyID]
	if !ok {
		return KeyMaterial{}, fmt.Errorf("unknown key")
	}
	return KeyMaterial{ID: keyID, Bytes: append([]byte(nil), key...)}, nil
}

func TestAESGCMCipherAuthenticatesEnvelopeHeader(t *testing.T) {
	provider := testKeyProvider{activeID: "local-v1", keys: map[string][]byte{
		"local-v1": bytes.Repeat([]byte{3}, 32),
	}}
	cipher, err := NewAESGCMCipher(provider)
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("checkpoint")
	ciphertext, err := cipher.Encrypt(context.Background(), []byte("secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	var envelope ciphertextEnvelope
	if err := json.Unmarshal(ciphertext, &envelope); err != nil {
		t.Fatal(err)
	}
	// A header rewritten to look like the version 1 format, which did not
	// authenticate the header, must not decrypt.
	envelope.Version = 1
	downgraded, _ := json.Marshal(envelope)
	if _, err := cipher.Decrypt(context.Background(), downgraded, aad); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("downgraded envelope error = %v, want ErrInvalidEnvelope", err)
	}
}

func TestAESGCMCipherReportsReplacedKey(t *testing.T) {
	keys := map[string][]byte{"local-v1": bytes.Repeat([]byte{4}, 32)}
	cipher, err := NewAESGCMCipher(testKeyProvider{activeID: "local-v1", keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.Encrypt(context.Background(), []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The keychain item was deleted and regenerated under the same id.
	keys["local-v1"] = bytes.Repeat([]byte{5}, 32)
	if _, err := cipher.Decrypt(context.Background(), ciphertext, nil); !errors.Is(err, ErrKeyMaterialChanged) {
		t.Fatalf("Decrypt with replaced key error = %v, want ErrKeyMaterialChanged", err)
	}
}
