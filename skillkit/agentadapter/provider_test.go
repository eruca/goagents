package agentadapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/skillkit"
)

var _ agentcore.SkillProvider = Provider{}

func TestProviderMapsActivatedSkillToAgentcoreSkill(t *testing.T) {
	activation := activatedSkill(t, "clinical-summary", "Use approved sources only.")
	provider := Provider{Resolve: func(context.Context, agentcore.RunRequest) (*skillkit.Activation, error) {
		return activation, nil
	}}

	skills, err := provider.Skills(context.Background(), agentcore.RunRequest{})
	if err != nil {
		t.Fatalf("Skills returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("skills = %#v, want one skill", skills)
	}
	if skills[0].Name != "clinical-summary" || skills[0].Description != "A bounded skill." || skills[0].Content != "Use approved sources only.\n" || !skills[0].Cacheable {
		t.Fatalf("skill = %#v, want mapped cacheable skill", skills[0])
	}
}

func TestProviderPropagatesResolverError(t *testing.T) {
	want := errors.New("activation failed")
	provider := Provider{Resolve: func(context.Context, agentcore.RunRequest) (*skillkit.Activation, error) {
		return nil, want
	}}

	_, err := provider.Skills(context.Background(), agentcore.RunRequest{})
	if !errors.Is(err, want) {
		t.Fatalf("Skills error = %v, want resolver error", err)
	}
}

func TestProviderRequiresResolver(t *testing.T) {
	_, err := (Provider{}).Skills(context.Background(), agentcore.RunRequest{})
	if !errors.Is(err, ErrMissingResolver) {
		t.Fatalf("Skills error = %v, want ErrMissingResolver", err)
	}
}

func TestProviderInjectsActivatedSkillIntoAgentPrompt(t *testing.T) {
	activation := activatedSkill(t, "clinical-summary", "Use approved sources only.")
	llm := &capturingLLM{}
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithSkillProvider(Provider{Resolve: func(context.Context, agentcore.RunRequest) (*skillkit.Activation, error) {
			return activation, nil
		}}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	if _, err := agent.Run(context.Background(), agentcore.RunRequest{Input: "summarize the note"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("LLM requests = %d, want one", len(llm.requests))
	}
	request := llm.requests[0]
	if len(request.Tools) != 0 {
		t.Fatalf("tools = %#v, want no tool registration from the adapter", request.Tools)
	}
	if len(request.Messages) < 2 {
		t.Fatalf("messages = %#v, want system skill and user input", request.Messages)
	}
	if got, want := request.Messages[0].Content, "Skill: clinical-summary\nDescription: A bounded skill.\nUse approved sources only.\n"; got != want {
		t.Fatalf("system prompt = %q, want %q", got, want)
	}
}

type capturingLLM struct {
	requests []ports.ChatRequest
}

func (m *capturingLLM) Chat(_ context.Context, request ports.ChatRequest) (*ports.ChatResponse, error) {
	m.requests = append(m.requests, request)
	return &ports.ChatResponse{Content: "done"}, nil
}

func activatedSkill(t *testing.T, name string, body string) *skillkit.Activation {
	t.Helper()
	root := t.TempDir()
	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	source := "---\nname: " + name + "\ndescription: A bounded skill.\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	catalog, err := skillkit.Discover([]skillkit.Root{{
		ID: "builtin", Dir: root, Scope: skillkit.ScopeBuiltin, Trusted: true, Enabled: true,
	}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	activation, err := catalog.Activate(skillkit.ActivationRequest{
		Skills: []skillkit.Ref{{Name: name}},
	})
	if err != nil {
		t.Fatalf("Activate returned error: %v", err)
	}
	return activation
}
