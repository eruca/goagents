//go:build darwin && cgo && !ios

package approvalcrypto

import (
	"fmt"
	"strings"

	macoskeychain "github.com/99designs/go-keychain"
)

const approvalKeychainLabel = "Goagents approval checkpoint data key"

// OpenMacOSKeychainKeyProvider opens the native macOS Keychain. It never
// falls back to a file, environment variable, or SQLite value.
func OpenMacOSKeychainKeyProvider(serviceName, activeKeyID string) (*keychainKeyProvider, error) {
	if strings.TrimSpace(serviceName) == "" {
		return nil, fmt.Errorf("Keychain service name is required")
	}
	return newKeychainKeyProvider(macosKeychainSecretStore{serviceName: serviceName}, activeKeyID)
}

type macosKeychainSecretStore struct {
	serviceName string
}

func (s macosKeychainSecretStore) Get(key string) ([]byte, error) {
	value, err := macoskeychain.GetGenericPassword(s.serviceName, key, approvalKeychainLabel, "")
	if err == macoskeychain.ErrorItemNotFound || len(value) == 0 && err == nil {
		return nil, errSecretNotFound
	}
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), value...), nil
}

func (s macosKeychainSecretStore) Set(key string, value []byte) error {
	item := macoskeychain.NewGenericPassword(s.serviceName, key, approvalKeychainLabel, append([]byte(nil), value...), "")
	item.SetSynchronizable(macoskeychain.SynchronizableNo)
	item.SetAccessible(macoskeychain.AccessibleWhenUnlockedThisDeviceOnly)
	if err := macoskeychain.AddItem(item); err == macoskeychain.ErrorDuplicateItem {
		return errSecretAlreadyExists
	} else {
		return err
	}
}
