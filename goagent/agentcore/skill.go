package agentcore

import "context"

type Skill struct {
	Name        string
	Description string
	Content     string
	Priority    int
	Cacheable   bool
}

type SkillProvider interface {
	Skills(ctx context.Context, req RunRequest) ([]Skill, error)
}
