package approvalcrypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func staticKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func mustStaticCipher(t *testing.T, activeID string, keys map[string][]byte) *AESGCMCipher {
	t.Helper()
	provider, err := NewStaticKeyProvider(activeID, keys)
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}
	cipher, err := NewAESGCMCipher(provider)
	if err != nil {
		t.Fatalf("NewAESGCMCipher: %v", err)
	}
	return cipher
}

func TestStaticKeyProviderRoundTrips(t *testing.T) {
	cipher := mustStaticCipher(t, "prod-v1", map[string][]byte{"prod-v1": staticKey(1)})
	aad := []byte("checkpoint-1")
	ciphertext, err := cipher.Encrypt(context.Background(), []byte("secret"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	plaintext, err := cipher.Decrypt(context.Background(), ciphertext, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plaintext) != "secret" {
		t.Fatalf("plaintext = %q", plaintext)
	}
}

func TestStaticKeyProviderRotation(t *testing.T) {
	old := mustStaticCipher(t, "prod-v1", map[string][]byte{"prod-v1": staticKey(1)})
	oldCiphertext, err := old.Encrypt(context.Background(), []byte("old"), nil)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := NewStaticKeyProvider("prod-v2", map[string][]byte{
		"prod-v1": staticKey(1),
		"prod-v2": staticKey(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := provider.Active(context.Background())
	if err != nil || active.ID != "prod-v2" || !bytes.Equal(active.Bytes, staticKey(2)) {
		t.Fatalf("Active = %#v, %v", active, err)
	}
	rotated, err := NewAESGCMCipher(provider)
	if err != nil {
		t.Fatal(err)
	}
	newCiphertext, err := rotated.Encrypt(context.Background(), []byte("new"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(newCiphertext, []byte(`"key_id":"prod-v2"`)) {
		t.Fatalf("new ciphertext not under prod-v2: %s", newCiphertext)
	}
	for want, ciphertext := range map[string][]byte{"old": oldCiphertext, "new": newCiphertext} {
		got, err := rotated.Decrypt(context.Background(), ciphertext, nil)
		if err != nil || string(got) != want {
			t.Fatalf("Decrypt %s = %q, %v", want, got, err)
		}
	}
	// The old-only keyring cannot read checkpoints written under the new key.
	if _, err := old.Decrypt(context.Background(), newCiphertext, nil); !errors.Is(err, ErrKeyMaterialUnavailable) {
		t.Fatalf("Decrypt with retired keyring error = %v, want ErrKeyMaterialUnavailable", err)
	}
}

func TestStaticKeyProviderResolveUnknownIsUnavailable(t *testing.T) {
	provider, err := NewStaticKeyProvider("prod-v1", map[string][]byte{"prod-v1": staticKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Resolve(context.Background(), "prod-v0"); !errors.Is(err, ErrKeyMaterialUnavailable) {
		t.Fatalf("Resolve unknown error = %v, want ErrKeyMaterialUnavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Active(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Active with canceled context error = %v", err)
	}
}

func TestNewStaticKeyProviderRejectsInvalidInput(t *testing.T) {
	short := bytes.Repeat([]byte{0xAB}, 31)
	cases := map[string]struct {
		activeID string
		keys     map[string][]byte
		want     error
	}{
		"empty active id":   {"", map[string][]byte{"v1": staticKey(1)}, ErrInvalidKeyProvider},
		"missing active id": {"v2", map[string][]byte{"v1": staticKey(1)}, ErrInvalidKeyProvider},
		"no keys":           {"v1", nil, ErrInvalidKeyProvider},
		"short key":         {"v1", map[string][]byte{"v1": short}, ErrInvalidKeyMaterial},
		"long key":          {"v1", map[string][]byte{"v1": bytes.Repeat([]byte{0xAB}, 33)}, ErrInvalidKeyMaterial},
		"blank key id":      {"v1", map[string][]byte{"v1": staticKey(1), "  ": staticKey(2)}, ErrInvalidKeyMaterial},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewStaticKeyProvider(tc.activeID, tc.keys)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			assertNoKeyMaterial(t, err, short)
		})
	}
}

func TestStaticKeyProviderIsolatedFromCallerMutation(t *testing.T) {
	key := staticKey(7)
	keys := map[string][]byte{"v1": key}
	provider, err := NewStaticKeyProvider("v1", keys)
	if err != nil {
		t.Fatal(err)
	}
	key[0] = 0xFF
	keys["v1"] = staticKey(8)
	keys["v2"] = staticKey(9)

	active, err := provider.Active(context.Background())
	if err != nil || !bytes.Equal(active.Bytes, staticKey(7)) {
		t.Fatalf("Active after input mutation = %#v, %v", active, err)
	}
	if _, err := provider.Resolve(context.Background(), "v2"); !errors.Is(err, ErrKeyMaterialUnavailable) {
		t.Fatalf("key added to input map became resolvable: %v", err)
	}

	active.Bytes[0] = 0xFF
	again, err := provider.Resolve(context.Background(), "v1")
	if err != nil || !bytes.Equal(again.Bytes, staticKey(7)) {
		t.Fatalf("Resolve after output mutation = %#v, %v", again, err)
	}
}

func TestStaticKeyProviderReportsReplacedKey(t *testing.T) {
	before := mustStaticCipher(t, "v1", map[string][]byte{"v1": staticKey(1)})
	ciphertext, err := before.Encrypt(context.Background(), []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Another replica (or a redeploy) supplies different bytes under the same id.
	after := mustStaticCipher(t, "v1", map[string][]byte{"v1": staticKey(2)})
	if _, err := after.Decrypt(context.Background(), ciphertext, nil); !errors.Is(err, ErrKeyMaterialChanged) {
		t.Fatalf("Decrypt with replaced key error = %v, want ErrKeyMaterialChanged", err)
	}
}

func TestParseKeyring(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(staticKey(1))
	k2 := base64.StdEncoding.EncodeToString(staticKey(2))
	keys, err := ParseKeyring("  prod-v1 : " + k1 + " ,\n prod-v2:" + k2 + "\n")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	if len(keys) != 2 || !bytes.Equal(keys["prod-v1"], staticKey(1)) || !bytes.Equal(keys["prod-v2"], staticKey(2)) {
		t.Fatalf("keys = %#v", keys)
	}
	provider, err := NewStaticKeyProvider("prod-v2", keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAESGCMCipher(provider); err != nil {
		t.Fatal(err)
	}
}

func TestParseKeyringRejectsInvalidSpecs(t *testing.T) {
	secret := staticKey(0x5A)
	good := base64.StdEncoding.EncodeToString(secret)
	short := base64.StdEncoding.EncodeToString(secret[:31])
	badBase64 := good[:20] + "!!" + good[22:]
	unpadded := base64.RawStdEncoding.EncodeToString(secret)
	urlSafe := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xFB}, 32))
	cases := map[string]string{
		"empty":           "",
		"whitespace":      "  \n ",
		"empty entry":     "v1:" + good + ",,v2:" + good,
		"trailing comma":  "v1:" + good + ",",
		"missing colon":   good,
		"missing id":      ":" + good,
		"missing key":     "v1:",
		"duplicate id":    "v1:" + good + ", v1 :" + good,
		"bad base64":      "v1:" + badBase64,
		"unpadded base64": "v1:" + unpadded,
		"url-safe base64": "v1:" + urlSafe,
		"wrong length":    "v1:" + short,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			keys, err := ParseKeyring(spec)
			if !errors.Is(err, ErrInvalidKeyMaterial) || keys != nil {
				t.Fatalf("ParseKeyring = %v, %v; want ErrInvalidKeyMaterial", keys, err)
			}
			assertNoKeyMaterial(t, err, secret)
			for _, encoded := range []string{good, short, badBase64, unpadded, urlSafe} {
				if strings.Contains(err.Error(), encoded[:16]) {
					t.Fatalf("error %q leaks base64 key material", err)
				}
			}
		})
	}
}

func assertNoKeyMaterial(t *testing.T, err error, key []byte) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, string(key[:8])) || strings.Contains(msg, base64.StdEncoding.EncodeToString(key)[:16]) {
		t.Fatalf("error %q leaks key material", msg)
	}
}
