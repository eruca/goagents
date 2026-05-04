package ports

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPortsDoesNotImportImplementationPackages(t *testing.T) {
	banned := map[string]bool{
		"github.com/eruca/goagent/policy": true,
		"github.com/eruca/goagent/prompt": true,
		"github.com/eruca/goagent/tools":  true,
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob ports files: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", file, err)
			}
			if banned[path] {
				t.Fatalf("%s imports implementation package %q", file, path)
			}
		}
	}
}
