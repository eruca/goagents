package runkit_test

import (
	"testing"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/runkit/storetest"
)

func TestMemoryCheckpointStoreConformance(t *testing.T) {
	storetest.RunCheckpointStoreConformance(t, "", func(*testing.T) runkit.CheckpointStore {
		return runkit.NewMemoryCheckpointStore()
	})
}
