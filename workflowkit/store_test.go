package workflowkit_test

import (
	"testing"

	"github.com/eruca/workflowkit"
	"github.com/eruca/workflowkit/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) workflowkit.Store {
		return workflowkit.NewMemoryStore()
	})
}

func TestMemoryQueueStoreConformance(t *testing.T) {
	storetest.RunQueueStoreConformance(t, func(t *testing.T) workflowkit.Store {
		return workflowkit.NewMemoryStore()
	})
}
