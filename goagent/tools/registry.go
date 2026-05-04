package tools

import (
	"errors"
	"fmt"
	"sort"
)

var ErrToolNotFound = errors.New("tool not found")

type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(tool Tool) {
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	r.tools[tool.Spec().Name] = tool
}

func (r *Registry) Clone() *Registry {
	clone := NewRegistry()
	for name, tool := range r.tools {
		clone.tools[name] = tool
	}
	return clone
}

func (r *Registry) Get(name string) (Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *Registry) Specs() []Spec {
	specs := make([]Spec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	sort.Slice(specs, func(i, j int) bool {
		return specs[i].Name < specs[j].Name
	})
	return specs
}

func (r *Registry) MustGet(name string) (Tool, error) {
	tool, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: tool %q not registered", ErrToolNotFound, name)
	}
	return tool, nil
}
