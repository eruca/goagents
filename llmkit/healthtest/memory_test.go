package healthtest_test

import (
	"testing"

	"github.com/eruca/goagents/llmkit"
	"github.com/eruca/goagents/llmkit/healthtest"
)

func TestMemoryHealthStoreConformance(t *testing.T) {
	healthtest.RunHealthStoreConformance(t, func(_ *testing.T, policy llmkit.HealthPolicy) (llmkit.HealthStore, func(llmkit.ProviderHealthEntry)) {
		store := llmkit.NewMemoryHealthStore(policy)
		return store, store.Set
	})
}
