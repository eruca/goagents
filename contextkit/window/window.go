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

const (
	kindKey             = "contextkit.kind"
	collapseIDKey       = "contextkit.collapse_id"
	droppedTotalKey     = "contextkit.dropped_total"
	kindWindowSummary   = "window_summary"
	kindCollapseSummary = "collapse_summary"
)

type Config struct {
	Budget contextkit.Budget
	// Profile defaults to contextkit.ProfileFromEnv.
	Profile contextkit.Profile
	// MinRecent is the number of trailing messages always kept, even over
	// budget; at least 1. The kept window then extends back to the start of
	// its turn.
	MinRecent    int
	MaxToolChars int
	Summarizer   contextkit.Summarizer
}

// Compressor keeps the newest whole turns that fit the budget and replaces
// everything older with one summary message. The budget covers system
// messages and the kept conversation; the summary is not counted, and a
// Summarizer receives the budget to size it.
//
// Compress is incremental: feed it its previous output plus the new messages
// and only the newly dropped messages are summarized, together with the
// previous summary, instead of the whole history again.
type Compressor struct {
	cfg Config
}

func New(cfg Config) *Compressor {
	if cfg.Profile == "" {
		cfg.Profile = contextkit.ProfileFromEnv()
	}
	if cfg.MinRecent <= 0 {
		cfg.MinRecent = 1
	}
	return &Compressor{cfg: cfg}
}

func (c *Compressor) Compress(ctx context.Context, req contextkit.Request) (*contextkit.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	profile := c.cfg.Profile
	if profile == "" {
		profile = contextkit.ProfileStandard
	}
	budget := effectiveBudget(req.Budget, c.cfg.Budget)

	messages := budgetToolMessages(req.Messages, c.cfg.MaxToolChars)
	system, previous, conversation := split(messages)
	available := budget.MaxChars - charLen(system)
	cut := windowStart(conversation, c.cfg.MinRecent, budget.MaxChars > 0, available)
	dropped, recent := conversation[:cut], conversation[cut:]

	result := &contextkit.Result{
		Profile: profile,
		Levels:  contextkit.LevelsForProfile(profile),
		Dropped: len(dropped),
	}
	out := append([]contextkit.Message(nil), system...)
	switch {
	case len(dropped) > 0:
		summary, err := c.summarize(ctx, profile, previous, dropped, recent, budget, req.Metadata, result)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
		result.Summary = summary.Content
	case len(previous) > 0:
		out = append(out, previous...)
		result.Summary = previous[len(previous)-1].Content
	}
	out = append(out, recent...)

	result.Messages = out
	result.OverBudget = budget.MaxChars > 0 && charLen(system)+charLen(recent) > budget.MaxChars
	return result, nil
}

// summarize builds the summary message that replaces the previous summary and
// the newly dropped messages. Deep mode also records a reversible collapse.
func (c *Compressor) summarize(ctx context.Context, profile contextkit.Profile, previous, dropped, recent []contextkit.Message, budget contextkit.Budget, metadata map[string]any, result *contextkit.Result) (contextkit.Message, error) {
	total := len(dropped)
	for _, msg := range previous {
		total += droppedTotal(msg)
	}
	summary := contextkit.Message{
		Role:     contextkit.RoleSystem,
		Content:  placeholderSummary(total, dropped),
		Metadata: map[string]any{kindKey: kindWindowSummary, droppedTotalKey: total},
	}
	if profile != contextkit.ProfileDeep {
		return summary, nil
	}

	if c.cfg.Summarizer != nil {
		compacted, err := c.cfg.Summarizer.Summarize(ctx, contextkit.SummarizeRequest{
			Previous:  joinContents(previous),
			Collapsed: cloneMessages(dropped),
			Recent:    cloneMessages(recent),
			Budget:    budget,
			Metadata:  cloneMetadata(metadata),
		})
		if err != nil {
			return contextkit.Message{}, err
		}
		if compacted != nil && compacted.Summary != "" {
			summary.Content = "summary of earlier context:\n" + compacted.Summary
			result.AutoCompact = &contextkit.Compact{Summary: compacted.Summary, Metadata: cloneMetadata(compacted.Metadata)}
		}
	}
	// The previous summary is an original too, so restoring this collapse
	// yields it back and an earlier collapse can then expand it in turn.
	collapse := buildCollapse(append(cloneMessages(previous), dropped...), summary)
	summary.Metadata[kindKey] = kindCollapseSummary
	summary.Metadata[collapseIDKey] = collapse.ID
	collapse.Summary = cloneMessage(summary)
	result.Collapses = []contextkit.Collapse{collapse}
	return summary, nil
}

// windowStart returns the index of the first kept conversation message. The
// kept window always holds the last minRecent messages, starts on a turn
// boundary so no tool reply loses its call, and, under a budget, extends back
// over earlier whole turns while they fit. Without a budget nothing is dropped.
func windowStart(messages []contextkit.Message, minRecent int, bounded bool, available int) int {
	if !bounded {
		return 0
	}
	boundaries := turnBoundaries(messages)
	limit := max(len(messages)-minRecent, 0)
	cut := 0
	for _, b := range boundaries {
		if b <= limit {
			cut = b
		}
	}
	suffix := charLen(messages[cut:])
	for i := len(boundaries) - 1; i >= 0; i-- {
		b := boundaries[i]
		if b >= cut {
			continue
		}
		suffix += charLen(messages[b:cut])
		if suffix > available {
			break
		}
		cut = b
	}
	return cut
}

// turnBoundaries lists the indexes a kept window may start at: each user
// message, or each non-tool message when the conversation has no user turns.
// Index 0 is always a boundary.
func turnBoundaries(messages []contextkit.Message) []int {
	hasUser := false
	for _, msg := range messages {
		if msg.Role == contextkit.RoleUser {
			hasUser = true
			break
		}
	}
	boundaries := []int{0}
	for i := 1; i < len(messages); i++ {
		role := messages[i].Role
		if role == contextkit.RoleUser || (!hasUser && role != contextkit.RoleTool) {
			boundaries = append(boundaries, i)
		}
	}
	return boundaries
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

// split separates real system messages and summaries left by an earlier
// Compress from the conversation.
func split(messages []contextkit.Message) (system, previous, conversation []contextkit.Message) {
	for _, msg := range messages {
		switch {
		case isSummary(msg):
			previous = append(previous, msg)
		case msg.Role == contextkit.RoleSystem:
			system = append(system, msg)
		default:
			conversation = append(conversation, msg)
		}
	}
	return system, previous, conversation
}

func isSummary(msg contextkit.Message) bool {
	kind, _ := msg.Metadata[kindKey].(string)
	return kind == kindWindowSummary || kind == kindCollapseSummary
}

// droppedTotal reads the count a summary carries, also after a JSON round
// trip has turned it into a float64.
func droppedTotal(msg contextkit.Message) int {
	switch n := msg.Metadata[droppedTotalKey].(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}

func placeholderSummary(total int, dropped []contextkit.Message) string {
	ids := make([]string, 0, len(dropped))
	for _, msg := range dropped {
		if msg.ID != "" {
			ids = append(ids, msg.ID)
		}
	}
	if len(ids) == 0 {
		return fmt.Sprintf("compressed earlier context: %d messages omitted", total)
	}
	return fmt.Sprintf("compressed earlier context: %d messages omitted, latest ids: %s", total, strings.Join(ids, ", "))
}

func joinContents(messages []contextkit.Message) string {
	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		contents = append(contents, msg.Content)
	}
	return strings.Join(contents, "\n")
}

func buildCollapse(messages []contextkit.Message, summary contextkit.Message) contextkit.Collapse {
	ids := make([]string, 0, len(messages))
	for i, msg := range messages {
		switch {
		case msg.ID != "":
			ids = append(ids, msg.ID)
		case isSummary(msg):
			id, _ := msg.Metadata[collapseIDKey].(string)
			ids = append(ids, "summary:"+id)
		default:
			ids = append(ids, fmt.Sprintf("index:%d", i))
		}
	}
	return contextkit.Collapse{
		ID:          collapseID(ids),
		StartIndex:  0,
		EndIndex:    len(messages) - 1,
		Summary:     summary,
		OriginalIDs: ids,
		Originals:   cloneMessages(messages),
	}
}

func collapseID(ids []string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(strings.Join(ids, "\x00")))
	return "collapse-" + hex.EncodeToString(h.Sum(nil))[:12]
}

func charLen(messages []contextkit.Message) int {
	total := 0
	for _, msg := range messages {
		total += msg.CharLen()
	}
	return total
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
