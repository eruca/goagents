package approvalcrypto

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

// StaticKeyProvider serves a fixed set of host-supplied 32-byte keys, for
// example a keyring read from a secret manager or environment variable. It
// never generates keys, so every replica given the same keyring encrypts and
// decrypts with identical key material. It is safe for concurrent use.
type StaticKeyProvider struct {
	activeID string
	keys     map[string][]byte
}

var _ KeyProvider = (*StaticKeyProvider)(nil)

// NewStaticKeyProvider validates and copies keys. Every key must be exactly
// 32 bytes and activeID must name one of them. Later changes to keys or its
// byte slices do not affect the provider.
func NewStaticKeyProvider(activeID string, keys map[string][]byte) (*StaticKeyProvider, error) {
	if strings.TrimSpace(activeID) == "" {
		return nil, fmt.Errorf("%w: active key id is required", ErrInvalidKeyProvider)
	}
	copied := make(map[string][]byte, len(keys))
	for id, key := range keys {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: key id is required", ErrInvalidKeyMaterial)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("%w: key %q must be 32 bytes, got %d", ErrInvalidKeyMaterial, id, len(key))
		}
		copied[id] = append([]byte(nil), key...)
	}
	if _, ok := copied[activeID]; !ok {
		return nil, fmt.Errorf("%w: active key %q is not in the keyring", ErrInvalidKeyProvider, activeID)
	}
	return &StaticKeyProvider{activeID: activeID, keys: copied}, nil
}

// Active returns a copy of the active key.
func (p *StaticKeyProvider) Active(ctx context.Context) (KeyMaterial, error) {
	if err := ctx.Err(); err != nil {
		return KeyMaterial{}, err
	}
	if p == nil {
		return KeyMaterial{}, ErrKeyMaterialUnavailable
	}
	return p.lookup(p.activeID)
}

// Resolve returns a copy of the key named by keyID, or an error wrapping
// ErrKeyMaterialUnavailable when the keyring does not contain it.
func (p *StaticKeyProvider) Resolve(ctx context.Context, keyID string) (KeyMaterial, error) {
	if err := ctx.Err(); err != nil {
		return KeyMaterial{}, err
	}
	if p == nil {
		return KeyMaterial{}, ErrKeyMaterialUnavailable
	}
	return p.lookup(keyID)
}

func (p *StaticKeyProvider) lookup(keyID string) (KeyMaterial, error) {
	key, ok := p.keys[keyID]
	if !ok {
		return KeyMaterial{}, fmt.Errorf("%w: key %q is not in the keyring", ErrKeyMaterialUnavailable, keyID)
	}
	return KeyMaterial{ID: keyID, Bytes: append([]byte(nil), key...)}, nil
}

// ParseKeyring parses "id1:base64,id2:base64", where each value is a
// standard-base64 encoding of a 32-byte key. Whitespace around entries, ids,
// and values is ignored. Errors name the offending entry or key id but never
// include key material.
func ParseKeyring(spec string) (map[string][]byte, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("%w: keyring is empty", ErrInvalidKeyMaterial)
	}
	keys := make(map[string][]byte)
	for i, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("%w: keyring entry %d is empty", ErrInvalidKeyMaterial, i+1)
		}
		id, encoded, ok := strings.Cut(entry, ":")
		id, encoded = strings.TrimSpace(id), strings.TrimSpace(encoded)
		if !ok || id == "" || encoded == "" {
			return nil, fmt.Errorf("%w: keyring entry %d must be id:base64", ErrInvalidKeyMaterial, i+1)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("%w: keyring entry %d repeats key id %q", ErrInvalidKeyMaterial, i+1, id)
		}
		key, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			// The decoder error is dropped so no part of the input is echoed.
			return nil, fmt.Errorf("%w: key %q is not valid standard base64", ErrInvalidKeyMaterial, id)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("%w: key %q must decode to 32 bytes, got %d", ErrInvalidKeyMaterial, id, len(key))
		}
		keys[id] = key
	}
	return keys, nil
}
