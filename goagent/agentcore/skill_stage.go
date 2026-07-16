package agentcore

import (
	"context"
	"strings"

	"github.com/eruca/goagents/goagent/prompt"
)

type SkillStage struct {
	Provider SkillProvider
}

func (s SkillStage) Name() string {
	return "skills"
}

func (s SkillStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Provider == nil || state.Metadata[skillsLoadedKey] == true {
		return StageContinue, nil
	}
	skills, err := s.Provider.Skills(ctx, state.Input)
	if err != nil {
		return StageAbort, err
	}
	for _, skill := range skills {
		if skill.Content == "" {
			continue
		}
		state.PromptBlocks = append(state.PromptBlocks, skillPromptBlock(skill))
	}
	state.Metadata[skillsLoadedKey] = true
	return StageContinue, nil
}

const skillsLoadedKey = "agentcore.skills.loaded"

func skillPromptBlock(skill Skill) prompt.Block {
	mode := prompt.ModeDynamic
	if skill.Cacheable {
		mode = prompt.ModeCacheable
	}
	return prompt.Block{
		Name:     "skill:" + skill.Name,
		Mode:     mode,
		Priority: skill.Priority,
		Content:  renderSkill(skill),
	}
}

func renderSkill(skill Skill) string {
	parts := make([]string, 0, 3)
	if skill.Name != "" {
		parts = append(parts, "Skill: "+skill.Name)
	}
	if skill.Description != "" {
		parts = append(parts, "Description: "+skill.Description)
	}
	parts = append(parts, skill.Content)
	return strings.Join(parts, "\n")
}
