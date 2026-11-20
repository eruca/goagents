package window

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/eruca/goagents/contextkit"
)

const (
	clearedKey   = "contextkit.tool_cleared"
	truncatedKey = "contextkit.truncated"
	// minTruncatedRunes is the shortest a field is cut to by the last-resort
	// step.
	minTruncatedRunes = 64
)

// clearReasoning clears the reasoning of all but the newest keep assistant
// messages that have any, and returns the originals of those it cleared.
func clearReasoning(conversation []contextkit.Message, keep int) ([]contextkit.Message, []contextkit.Message) {
	var withReasoning []int
	for index, msg := range conversation {
		if msg.Role == contextkit.RoleAssistant && msg.Reasoning != "" {
			withReasoning = append(withReasoning, index)
		}
	}
	if len(withReasoning) <= keep {
		return conversation, nil
	}
	out := cloneMessages(conversation)
	var cleared []contextkit.Message
	for _, index := range withReasoning[:len(withReasoning)-keep] {
		cleared = append(cleared, cloneMessage(conversation[index]))
		out[index].Reasoning = ""
	}
	return out, cleared
}

// clearToolResults clears tool results oldest first, all but the newest
// KeepRecentToolResults, until the conversation fits, and returns the
// originals of those it cleared. A cleared result keeps its tool call ID,
// so it still answers its call, and its reference, offloading its content
// first when an OffloadStore is set.
func (c *Compressor) clearToolResults(ctx context.Context, conversation []contextkit.Message, fits func([]contextkit.Message) bool) ([]contextkit.Message, []contextkit.Message) {
	var results []int
	for index, msg := range conversation {
		if msg.Role == contextkit.RoleTool && msg.Metadata[clearedKey] != true {
			results = append(results, index)
		}
	}
	if len(results) <= c.cfg.KeepRecentToolResults {
		return conversation, nil
	}
	out := cloneMessages(conversation)
	var cleared []contextkit.Message
	for _, index := range results[:len(results)-c.cfg.KeepRecentToolResults] {
		original := out[index]
		cleared = append(cleared, cloneMessage(original))
		out[index] = clearedToolResult(original, c.offload(ctx, original))
		if fits(out) {
			break
		}
	}
	return out, cleared
}

func clearedToolResult(msg contextkit.Message, ref string) contextkit.Message {
	chars := len([]rune(msg.Content))
	switch n := msg.Metadata["contextkit.tool_original_chars"].(type) {
	case int:
		chars = n
	case float64:
		chars = int(n)
	}
	toolName, status := msg.ToolName, msg.Status
	if toolName == "" {
		toolName = "unknown"
	}
	if status == "" {
		status = "success"
	}
	lines := []string{"tool=" + toolName, "status=" + status}
	if msg.ToolCallID != "" {
		lines = append(lines, "tool_call_id="+msg.ToolCallID)
	}
	note := fmt.Sprintf("[older result cleared: %d chars]", chars)
	if ref != "" {
		lines = append(lines, "ref="+ref)
		note = fmt.Sprintf("[older result cleared: %d chars; read ref %s to see it again]", chars, ref)
	}
	lines = append(lines, "result="+note)
	cleared := cloneMessage(msg)
	cleared.Content = strings.Join(lines, "\n")
	cleared.Ref = ref
	if cleared.Metadata == nil {
		cleared.Metadata = map[string]any{}
	}
	cleared.Metadata["contextkit.tool_projected"] = true
	cleared.Metadata[clearedKey] = true
	return cleared
}

// truncateToFit is the last resort when the kept messages alone do not fit:
// it halves the longest tool result or reasoning among them until they fit
// or nothing long is left, and returns the originals of those it shortened.
// User messages and assistant answers are never cut.
func truncateToFit(messages []contextkit.Message, fits func([]contextkit.Message) bool) ([]contextkit.Message, []contextkit.Message) {
	out := cloneMessages(messages)
	var originals []contextkit.Message
	touched := map[int]bool{}
	for !fits(out) {
		index, field, longest := -1, "", minTruncatedRunes
		for i, msg := range out {
			if msg.Role == contextkit.RoleTool {
				if n := len([]rune(msg.Content)); n > longest {
					index, field, longest = i, "content", n
				}
			}
			if msg.Role == contextkit.RoleAssistant {
				if n := len([]rune(msg.Reasoning)); n > longest {
					index, field, longest = i, "reasoning", n
				}
			}
		}
		if index < 0 {
			break
		}
		if !touched[index] {
			touched[index] = true
			originals = append(originals, cloneMessage(messages[index]))
		}
		msg := &out[index]
		if field == "content" {
			msg.Content = halve(msg.Content)
		} else {
			msg.Reasoning = halve(msg.Reasoning)
		}
		if msg.Metadata == nil {
			msg.Metadata = map[string]any{}
		}
		msg.Metadata[truncatedKey] = true
	}
	return out, originals
}

func halve(text string) string {
	runes := []rune(text)
	keep := max(len(runes)/2, minTruncatedRunes)
	return string(runes[:keep]) + fmt.Sprintf("\n[truncated %d chars]", len(runes)-keep)
}

// summarizerReady reports whether this compression may call the
// Summarizer: deep mode, one is set, and it is not paused after failures.
func (c *Compressor) summarizerReady(profile contextkit.Profile) bool {
	if profile != contextkit.ProfileDeep || c.cfg.Summarizer == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.cfg.Now().Before(c.pausedUntil)
}

// recordSummarizer counts consecutive failures and pauses the Summarizer
// for SummarizerCooldown after SummarizerFailureLimit of them.
func (c *Compressor) recordSummarizer(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.failures = 0
		return
	}
	c.failures++
	if c.failures >= c.cfg.SummarizerFailureLimit {
		c.failures = 0
		c.pausedUntil = c.cfg.Now().Add(c.cfg.SummarizerCooldown)
	}
}

// dependencyContextError returns the caller's context error when the
// Summarizer failed because the context ended.
func dependencyContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// scale is the budget with each limit multiplied by factor, at least 1.
func (b limits) scale(factor float64) limits {
	scaled := b
	if b.MaxChars > 0 {
		scaled.MaxChars = max(int(float64(b.MaxChars)*factor), 1)
	}
	if b.MaxTokens > 0 {
		scaled.MaxTokens = max(int(float64(b.MaxTokens)*factor), 1)
	}
	return scaled
}
