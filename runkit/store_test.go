package runkit_test

import (
	"testing"

	"github.com/eruca/runkit"
	"github.com/eruca/runkit/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) runkit.Store {
		return runkit.NewMemoryStore()
	})
}
