package artifactkit_test

import (
	"testing"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/artifactkit/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) artifactkit.Store {
		return artifactkit.NewMemoryStore()
	})
}
