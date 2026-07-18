//go:build provideracceptance

package main

import (
	"strings"
	"testing"
)

func TestLoadRealProviderSentinels(t *testing.T) {
	values := map[string]string{
		realProviderPromptSentinelEnv:      "prompt-value",
		realProviderObservationSentinelEnv: "observation-value",
		realProviderResponseSentinelEnv:    "response-value",
	}
	getenv := func(key string) string {
		return values[key]
	}

	sentinels, err := loadRealProviderSentinels(getenv)
	if err != nil {
		t.Fatalf("load real Provider sentinels: %v", err)
	}
	if sentinels.prompt != values[realProviderPromptSentinelEnv] ||
		sentinels.observation != values[realProviderObservationSentinelEnv] ||
		sentinels.response != values[realProviderResponseSentinelEnv] {
		t.Fatal("loaded real Provider sentinels do not match their labeled environment values")
	}

	for _, test := range []struct {
		name  string
		key   string
		label string
	}{
		{name: "prompt", key: realProviderPromptSentinelEnv, label: "prompt"},
		{name: "observation", key: realProviderObservationSentinelEnv, label: "observation"},
		{name: "response", key: realProviderResponseSentinelEnv, label: "response"},
	} {
		t.Run("missing_"+test.name, func(t *testing.T) {
			original := values[test.key]
			values[test.key] = ""
			t.Cleanup(func() {
				values[test.key] = original
			})

			_, err := loadRealProviderSentinels(getenv)
			if err == nil || !strings.Contains(err.Error(), test.label) {
				t.Fatalf("missing %s sentinel did not fail closed with its stable label", test.name)
			}
			for _, value := range values {
				if value != "" && strings.Contains(err.Error(), value) {
					t.Fatalf("missing %s sentinel error exposed a configured sentinel", test.name)
				}
			}
		})
	}
}
