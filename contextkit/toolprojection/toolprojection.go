package toolprojection

import (
	"fmt"
	"strings"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/toolbudget"
)

type Config struct {
	MaxResultChars int
}

const projectedKey = "contextkit.tool_projected"

func Project(msg contextkit.Message, cfg Config) contextkit.Message {
	out := cloneMessage(msg)
	// A projected message is returned as is, so feeding a compressor's output
	// back in, as incremental compression does, never wraps it again.
	if out.Role != contextkit.RoleTool || out.Metadata[projectedKey] == true {
		return out
	}

	toolName := out.ToolName
	if toolName == "" {
		toolName = "unknown"
	}
	status := out.Status
	if status == "" {
		status = "success"
	}

	budgeted := toolbudget.Apply(out.Content, toolbudget.Config{MaxChars: cfg.MaxResultChars})
	lines := []string{
		"tool=" + toolName,
		"status=" + status,
	}
	if out.ToolCallID != "" {
		lines = append(lines, "tool_call_id="+out.ToolCallID)
	}
	if out.Ref != "" {
		lines = append(lines, "ref="+out.Ref)
	}
	lines = append(lines, "result="+budgeted.Content)
	out.Content = strings.Join(lines, "\n")
	out.Metadata = ensureMetadata(out.Metadata)
	out.Metadata[projectedKey] = true
	if budgeted.Truncated {
		out.Metadata["contextkit.tool_truncated"] = true
		out.Metadata["contextkit.tool_omitted_chars"] = budgeted.OmittedChars
	}
	out.Metadata["contextkit.tool_original_chars"] = budgeted.OriginalChars
	return out
}

func cloneMessage(msg contextkit.Message) contextkit.Message {
	if len(msg.Metadata) == 0 {
		msg.Metadata = nil
		return msg
	}
	copied := make(map[string]any, len(msg.Metadata))
	for k, v := range msg.Metadata {
		copied[k] = v
	}
	msg.Metadata = copied
	return msg
}

func ensureMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return make(map[string]any)
	}
	return metadata
}

func FormatResultRef(toolName, id string) string {
	if id == "" {
		return ""
	}
	if toolName == "" {
		return id
	}
	return fmt.Sprintf("%s:%s", toolName, id)
}
