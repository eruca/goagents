package approvalcrypto

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestKeychainProviderCreatesActiveKeyOnce(t *testing.T) {
	store := &memorySecretStore{values: make(map[string][]byte)}
	provider, err := newKeychainKeyProvider(store, "local-v1")
	if err != nil {
		t.Fatalf("newKeychainKeyProvider: %v", err)
	}
	first, err := provider.Active(context.Background())
	if err != nil {
		t.Fatalf("first Active: %v", err)
	}
	second, err := provider.Active(context.Background())
	if err != nil {
		t.Fatalf("second Active: %v", err)
	}
	if first.ID != "local-v1" || len(first.Bytes) != 32 || !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatalf("active keys = %#v, %#v", first, second)
	}
	if len(store.values) != 1 {
		t.Fatalf("stored keys = %#v, want one", store.values)
	}
}

func TestKeychainProviderResolvesPriorKeyIDWithoutCreatingUnknownKey(t *testing.T) {
	store := &memorySecretStore{values: map[string][]byte{
		approvalKeychainItemName("local-v0"): bytes.Repeat([]byte{9}, 32),
	}}
	provider, err := newKeychainKeyProvider(store, "local-v1")
	if err != nil {
		t.Fatalf("newKeychainKeyProvider: %v", err)
	}
	prior, err := provider.Resolve(context.Background(), "local-v0")
	if err != nil {
		t.Fatalf("Resolve prior key: %v", err)
	}
	if prior.ID != "local-v0" || !bytes.Equal(prior.Bytes, bytes.Repeat([]byte{9}, 32)) {
		t.Fatalf("prior key = %#v", prior)
	}
	_, err = provider.Resolve(context.Background(), "unknown")
	if !errors.Is(err, ErrKeyMaterialUnavailable) {
		t.Fatalf("Resolve unknown error = %v, want ErrKeyMaterialUnavailable", err)
	}
	if len(store.values) != 1 {
		t.Fatalf("unknown resolve created a key: %#v", store.values)
	}
}

type memorySecretStore struct {
	values map[string][]byte
}

func (s *memorySecretStore) Get(key string) ([]byte, error) {
	value, ok := s.values[key]
	if !ok {
		return nil, errSecretNotFound
	}
	return append([]byte(nil), value...), nil
}

func (s *memorySecretStore) Set(key string, value []byte) error {
	s.values[key] = append([]byte(nil), value...)
	return nil
}
