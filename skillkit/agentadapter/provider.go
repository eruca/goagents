// Package agentadapter adapts host-approved skillkit activations to the
// goagent SkillProvider contract. It never registers or executes tools.
package agentadapter

import (
	"context"
	"errors"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/skillkit"
)

var (
	ErrMissingResolver   = errors.New("skill activation resolver is required")
	ErrMissingActivation = errors.New("skill activation is required")
)

// ActivationResolver resolves the host-selected activation for one agent run.
// The host remains responsible for deriving its tool policy from that same
// activation before it creates the run-scoped tool registry.
type ActivationResolver func(context.Context, agentcore.RunRequest) (*skillkit.Activation, error)

// Provider maps an already authorized activation to model-facing skills.
type Provider struct {
	Resolve ActivationResolver
}

// Skills implements agentcore.SkillProvider without changing the run tool set.
func (p Provider) Skills(ctx context.Context, request agentcore.RunRequest) ([]agentcore.Skill, error) {
	if p.Resolve == nil {
		return nil, ErrMissingResolver
	}
	activation, err := p.Resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	if activation == nil {
		return nil, ErrMissingActivation
	}
	activated := activation.Skills()
	skills := make([]agentcore.Skill, len(activated))
	for index, skill := range activated {
		skills[index] = agentcore.Skill{
			Name:        skill.Name,
			Description: skill.Description,
			Content:     skill.Content,
			Cacheable:   true,
		}
	}
	return skills, nil
}
