package agentcore

import (
	"fmt"
	"sort"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

// MutableToolRegistry is the per-run registry shape used by stages that both
// read configured tools and register request-scoped tools.
type MutableToolRegistry interface {
	ports.ToolRegistry
	Register(tool tools.Tool)
}

type runToolRegistry struct {
	base   ports.ToolRegistry
	scoped *tools.Registry
}

func (r *runToolRegistry) Register(tool tools.Tool) {
	r.scoped.Register(tool)
}

func (r *runToolRegistry) Get(name string) (tools.Tool, bool) {
	if tool, ok := r.scoped.Get(name); ok {
		return tool, true
	}
	return r.base.Get(name)
}

func (r *runToolRegistry) MustGet(name string) (tools.Tool, error) {
	if tool, ok := r.Get(name); ok {
		return tool, nil
	}
	return nil, fmt.Errorf("%w: tool %q not registered", tools.ErrToolNotFound, name)
}

func (r *runToolRegistry) Specs() []tools.Spec {
	byName := make(map[string]tools.Spec)
	for _, spec := range r.base.Specs() {
		byName[spec.Name] = spec
	}
	for _, spec := range r.scoped.Specs() {
		byName[spec.Name] = spec
	}

	specs := make([]tools.Spec, 0, len(byName))
	for _, spec := range byName {
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool {
		return specs[i].Name < specs[j].Name
	})
	return specs
}
