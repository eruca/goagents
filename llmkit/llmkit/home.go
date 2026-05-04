package llmkit

import (
	"fmt"
	"os"
	"path/filepath"
)

// HomeMode controls whether ResolveHome may use development conveniences.
type HomeMode string

const (
	HomeModeDevelopment HomeMode = "development"
	HomeModeProduction  HomeMode = "production"
)

// ResolveHome resolves llmkit's configuration home without creating it.
func ResolveHome(cwd string, getenv func(string) string, mode HomeMode) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	if home := getenv("LLMKIT_HOME"); home != "" {
		return cleanHome(cwd, home), nil
	}

	switch mode {
	case HomeModeDevelopment:
		home := filepath.Join(cwd, ".llmkit")
		info, err := os.Stat(home)
		if err == nil && info.IsDir() {
			return filepath.Clean(home), nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve llmkit development home: %w", err)
		}
		return "", fmt.Errorf("LLMKIT_HOME is required when %s does not exist", home)
	case HomeModeProduction:
		return "", fmt.Errorf("LLMKIT_HOME is required in production mode")
	default:
		return "", fmt.Errorf("unsupported llmkit home mode %q", mode)
	}
}

func cleanHome(cwd, home string) string {
	if filepath.IsAbs(home) {
		return filepath.Clean(home)
	}
	return filepath.Clean(filepath.Join(cwd, home))
}
