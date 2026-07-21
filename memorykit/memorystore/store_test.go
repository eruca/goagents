package memorystore

import (
	"testing"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/storetest"
)

func TestNewRejectsInvalidLimits(t *testing.T) {
	if _, err := New(memorykit.Limits{}); err == nil {
		t.Fatal("New accepted empty limits")
	}
}

func TestLifecycleConformance(t *testing.T) {
	storetest.RunLifecycleConformance(t, func(t *testing.T) memorykit.LifecycleStore {
		t.Helper()
		store, err := New(memorykit.Limits{
			Version:     "project-memory-v1",
			MaxKeyRunes: 128, MaxContentRunes: 4096, MaxMetadataRunes: 512,
			MaxSourcesPerMemory: 8, MaxListItems: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}
