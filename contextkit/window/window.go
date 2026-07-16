package window

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/toolprojection"
)

type Config struct {
	Budget       contextkit.Budget
	Profile      contextkit.Profile
	MinRecent    int
	MaxToolChars int
	Summarizer   contextkit.Summarizer
}

type Compressor struct {
	cfg Config
}

func New(cfg Config) *Compressor {
	if cfg.Profile == "" {
		cfg.Profile = contextkit.ProfileFromEnv()
	}
	if cfg.MinRecent <= 0 {
		cfg.MinRecent = 4
	}
	return &Compressor{cfg: cfg}
}

func (c *Compressor) Compress(ctx context.Context, req contextkit.Request) (*contextkit.Result, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	profile := c.cfg.Profile
	if profile == "" {
		profile = contextkit.ProfileStandard
	}

	messages := budgetToolMessages(req.Messages, c.cfg.MaxToolChars)
	system, nonSystem := splitSystem(messages)
	recent, dropped := keepRecent(nonSystem, c.cfg.MinRecent)

	budget := effectiveBudget(req.Budget, c.cfg.Budget)
	out := append([]contextkit.Message(nil), system...)
	summary := ""
	var collapse *contextkit.Collapse
	if len(dropped) > 0 {
		summary = summarizeDropped(dropped)
		if profile == contextkit.ProfileDeep {
			if compacted, err := c.compactDropped(ctx, dropped, recent, budget, req.Metadata); err != nil {
				return nil, err
			} else if compacted != nil && compacted.Summary != "" {
				summary = "auto compacted earlier context:\n" + compacted.Summary
			}
			built := buildCollapse(dropped, summary)
			collapse = &built
		}
		metadata := map[string]any{
			"contextkit.kind": "window_summary",
		}
		if collapse != nil {
			metadata["contextkit.kind"] = "collapse_summary"
			metadata["contextkit.collapse_id"] = collapse.ID
		}
		out = append(out, contextkit.Message{
			Role:     contextkit.RoleAssistant,
			Content:  summary,
			Metadata: metadata,
		})
	}
	out = append(out, recent...)

	result := &contextkit.Result{
		Messages: out,
		Profile:  profile,
		Levels:   contextkit.LevelsForProfile(profile),
		Summary:  summary,
		Dropped:  len(dropped),
	}

	if profile == contextkit.ProfileDeep && collapse != nil {
		result.Collapses = []contextkit.Collapse{*collapse}
		if c.cfg.Summarizer != nil {
			result.AutoCompact = &contextkit.Compact{
				Summary: strings.TrimPrefix(summary, "auto compacted earlier context:\n"),
			}
		} else if exceedsBudget(out, budget) {
			result.AutoCompact = &contextkit.Compact{
				Summary: fmt.Sprintf("deep compact: preserved %d messages, collapsed %d earlier messages", len(out), len(dropped)),
			}
		} else {
			result.AutoCompact = &contextkit.Compact{
				Summary: fmt.Sprintf("deep compact: collapse %s available for recovery", collapse.ID),
			}
		}
	}

	return result, nil
}

func (c *Compressor) compactDropped(ctx context.Context, dropped []contextkit.Message, recent []contextkit.Message, budget contextkit.Budget, metadata map[string]any) (*contextkit.SummarizeResult, error) {
	if c.cfg.Summarizer == nil {
		return nil, nil
	}
	return c.cfg.Summarizer.Summarize(ctx, contextkit.SummarizeRequest{
		Collapsed: cloneMessages(dropped),
		Recent:    cloneMessages(recent),
		Budget:    budget,
		Metadata:  cloneMetadata(metadata),
	})
}

func budgetToolMessages(messages []contextkit.Message, maxToolChars int) []contextkit.Message {
	out := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		copied := cloneMessage(msg)
		if copied.Role == contextkit.RoleTool {
			copied = toolprojection.Project(copied, toolprojection.Config{MaxResultChars: maxToolChars})
		}
		out = append(out, copied)
	}
	return out
}

func splitSystem(messages []contextkit.Message) ([]contextkit.Message, []contextkit.Message) {
	system := make([]contextkit.Message, 0)
	rest := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == contextkit.RoleSystem {
			system = append(system, msg)
			continue
		}
		rest = append(rest, msg)
	}
	return system, rest
}

func keepRecent(messages []contextkit.Message, minRecent int) ([]contextkit.Message, []contextkit.Message) {
	if len(messages) <= minRecent {
		return append([]contextkit.Message(nil), messages...), nil
	}
	cut := len(messages) - minRecent
	return append([]contextkit.Message(nil), messages[cut:]...), append([]contextkit.Message(nil), messages[:cut]...)
}

func summarizeDropped(messages []contextkit.Message) string {
	var roles []string
	ids := make([]string, 0, len(messages))
	for _, msg := range messages {
		roles = append(roles, string(msg.Role))
		if msg.ID != "" {
			ids = append(ids, msg.ID)
		}
	}
	if len(ids) > 0 {
		return fmt.Sprintf("compressed earlier context: %d messages (%s), ids: %s", len(messages), strings.Join(roles, ", "), strings.Join(ids, ", "))
	}
	return fmt.Sprintf("compressed earlier context: %d messages (%s)", len(messages), strings.Join(roles, ", "))
}

func buildCollapse(messages []contextkit.Message, summary string) contextkit.Collapse {
	ids := make([]string, 0, len(messages))
	for i, msg := range messages {
		if msg.ID != "" {
			ids = append(ids, msg.ID)
			continue
		}
		ids = append(ids, fmt.Sprintf("index:%d", i))
	}
	return contextkit.Collapse{
		ID:          collapseID(ids),
		StartIndex:  0,
		EndIndex:    len(messages) - 1,
		Summary:     contextkit.Message{Role: contextkit.RoleAssistant, Content: summary},
		OriginalIDs: ids,
		Originals:   cloneMessages(messages),
	}
}

func collapseID(ids []string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(strings.Join(ids, "\x00")))
	return "collapse-" + hex.EncodeToString(h.Sum(nil))[:12]
}

func exceedsBudget(messages []contextkit.Message, budget contextkit.Budget) bool {
	if budget.MaxChars <= 0 {
		return false
	}
	total := 0
	for _, msg := range messages {
		total += msg.CharLen()
	}
	return total > budget.MaxChars
}

func effectiveBudget(requestBudget, configBudget contextkit.Budget) contextkit.Budget {
	if requestBudget.MaxChars > 0 {
		return requestBudget
	}
	return configBudget
}

func cloneMessage(msg contextkit.Message) contextkit.Message {
	msg.Metadata = cloneMetadata(msg.Metadata)
	return msg
}

func cloneMessages(messages []contextkit.Message) []contextkit.Message {
	out := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, cloneMessage(msg))
	}
	return out
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	copied := make(map[string]any, len(metadata))
	for k, v := range metadata {
		copied[k] = v
	}
	return copied
}
