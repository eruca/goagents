package approvalcrypto

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrKeyMaterialUnavailable = errors.New("approval key material unavailable")
	errSecretNotFound         = errors.New("approval secret not found")
	errSecretAlreadyExists    = errors.New("approval secret already exists")
)

const approvalKeychainItemPrefix = "approval-data-key:"

type keychainKeyProvider struct {
	secrets     secretStore
	activeKeyID string
}

func newKeychainKeyProvider(secrets secretStore, activeKeyID string) (*keychainKeyProvider, error) {
	if secrets == nil || strings.TrimSpace(activeKeyID) == "" {
		return nil, ErrInvalidKeyProvider
	}
	return &keychainKeyProvider{secrets: secrets, activeKeyID: activeKeyID}, nil
}

func (p *keychainKeyProvider) Active(ctx context.Context) (KeyMaterial, error) {
	if err := ctx.Err(); err != nil {
		return KeyMaterial{}, err
	}
	return p.load(p.activeKeyID, true)
}

func (p *keychainKeyProvider) Resolve(ctx context.Context, keyID string) (KeyMaterial, error) {
	if err := ctx.Err(); err != nil {
		return KeyMaterial{}, err
	}
	return p.load(keyID, false)
}

func (p *keychainKeyProvider) load(keyID string, create bool) (KeyMaterial, error) {
	if p == nil || p.secrets == nil || strings.TrimSpace(keyID) == "" {
		return KeyMaterial{}, ErrKeyMaterialUnavailable
	}
	itemName := approvalKeychainItemName(keyID)
	key, err := p.secrets.Get(itemName)
	if errors.Is(err, errSecretNotFound) && create {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return KeyMaterial{}, fmt.Errorf("generate approval data key: %w", err)
		}
		if err := p.secrets.Set(itemName, key); err != nil {
			if errors.Is(err, errSecretAlreadyExists) {
				return p.load(keyID, false)
			}
			return KeyMaterial{}, fmt.Errorf("%w: store active key", ErrKeyMaterialUnavailable)
		}
	} else if err != nil {
		return KeyMaterial{}, fmt.Errorf("%w: read key", ErrKeyMaterialUnavailable)
	}
	material := KeyMaterial{ID: keyID, Bytes: append([]byte(nil), key...)}
	if _, err := newAEAD(material); err != nil {
		return KeyMaterial{}, ErrKeyMaterialUnavailable
	}
	return material, nil
}

func approvalKeychainItemName(keyID string) string {
	return approvalKeychainItemPrefix + keyID
}

type secretStore interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
}
