//go:build !darwin || !cgo || ios

package approvalcrypto

import "fmt"

// OpenMacOSKeychainKeyProvider is unavailable outside a local macOS host.
func OpenMacOSKeychainKeyProvider(serviceName, activeKeyID string) (*keychainKeyProvider, error) {
	return nil, fmt.Errorf("%w: macOS Keychain is unavailable", ErrKeyMaterialUnavailable)
}
