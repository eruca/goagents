package llmkit

import (
	"path/filepath"
	"testing"
)

func TestResolveHomeUsesLLMKITHomeWhenSet(t *testing.T) {
	cwd := t.TempDir()

	got, err := ResolveHome(cwd, func(key string) string {
		if key == "LLMKIT_HOME" {
			return "configs/../home"
		}
		return ""
	}, HomeModeProduction)
	if err != nil {
		t.Fatalf("ResolveHome returned error: %v", err)
	}

	want := filepath.Join(cwd, "home")
	if got != want {
		t.Fatalf("ResolveHome = %q, want %q", got, want)
	}
}

func TestResolveHomeUsesDevelopmentDirectoryWhenEnvMissingWithoutExistingDirectory(t *testing.T) {
	cwd := t.TempDir()
	llmkitHome := filepath.Join(cwd, ".llmkit")

	got, err := ResolveHome(cwd, func(string) string {
		return ""
	}, HomeModeDevelopment)
	if err != nil {
		t.Fatalf("ResolveHome returned error: %v", err)
	}

	if got != llmkitHome {
		t.Fatalf("ResolveHome = %q, want %q", got, llmkitHome)
	}
}

func TestResolveHomeProductionRequiresLLMKITHome(t *testing.T) {
	cwd := t.TempDir()

	got, err := ResolveHome(cwd, func(string) string {
		return ""
	}, HomeModeProduction)
	if err == nil {
		t.Fatalf("ResolveHome = %q, want error", got)
	}
}
