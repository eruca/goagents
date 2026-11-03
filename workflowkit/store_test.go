package workflowkit_test

import (
	"testing"

	"github.com/eruca/goagents/workflowkit"
	"github.com/eruca/goagents/workflowkit/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) workflowkit.Store {
		return workflowkit.NewMemoryStore()
	})
}

func TestMemoryQueueRecoveryStoreConformance(t *testing.T) {
	storetest.RunQueueRecoveryStoreConformance(t, func(t *testing.T) workflowkit.Store {
		return workflowkit.NewMemoryStore()
	})
}

func TestMemoryWorkflowQueryStoreConformance(t *testing.T) {
	storetest.RunWorkflowQueryStoreConformance(t, func(t *testing.T) workflowkit.Store {
		return workflowkit.NewMemoryStore()
	})
}
