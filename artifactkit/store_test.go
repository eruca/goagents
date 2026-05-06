package artifactkit_test

import (
	"testing"

	"github.com/eruca/artifactkit"
	"github.com/eruca/artifactkit/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) artifactkit.Store {
		return artifactkit.NewMemoryStore()
	})
}
