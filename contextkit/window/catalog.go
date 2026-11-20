package window

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/eruca/goagents/contextkit"
)

const (
	// catalogShare bounds the catalog to this share of the budget, so a
	// small budget keeps conversation rather than listing what it dropped.
	catalogShare        = 0.1
	catalogSnippetRunes = 80
	catalogArgsRunes    = 60
	assumedRef          = "ctx:0000000000000000"
)

var unlistedPattern = regexp.MustCompile(`^- \((\d+) earlier entries not listed\)$`)

// catalog lists the dropped steps, newest last: a user or assistant message
// by its opening words, and a tool call by its name, its arguments, and the
// size of its result, each with the reference it can be read back by. It
// keeps the entries of the previous summaries' catalogs and the newest
// CatalogEntries of all, noting how many older ones it leaves out.
func (c *Compressor) catalog(previous, dropped []contextkit.Message, assumeRefs bool, fits func(string) bool) string {
	var entries []string
	unlisted := 0
	for _, msg := range previous {
		earlier, more := catalogEntries(msg.Content)
		entries = append(entries, earlier...)
		unlisted += more
	}
	entries = append(entries, droppedEntries(dropped, assumeRefs)...)
	if len(entries) == 0 {
		return ""
	}
	if extra := len(entries) - c.cfg.CatalogEntries; extra > 0 {
		unlisted += extra
		entries = entries[extra:]
	}
	for len(entries) > 0 {
		if rendered := renderCatalog(entries, unlisted, assumeRefs); fits(rendered) {
			return rendered
		}
		entries = entries[1:]
		unlisted++
	}
	return ""
}

func renderCatalog(entries []string, unlisted int, assumeRefs bool) string {
	header := catalogHeader + "):"
	if assumeRefs || hasRef(entries) {
		header = catalogHeader + "; read a ref to see it again):"
	}
	lines := []string{header}
	if unlisted > 0 {
		lines = append(lines, fmt.Sprintf("- (%d earlier entries not listed)", unlisted))
	}
	return strings.Join(append(lines, entries...), "\n")
}

// catalogEntries reads back the entries a summary's catalog lists and the
// count of those it left out.
func catalogEntries(content string) ([]string, int) {
	index := strings.Index(content, catalogHeader)
	if index < 0 {
		return nil, 0
	}
	var entries []string
	unlisted := 0
	for _, line := range strings.Split(content[index:], "\n")[1:] {
		if match := unlistedPattern.FindStringSubmatch(line); match != nil {
			n, _ := strconv.Atoi(match[1])
			unlisted += n
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			break
		}
		entries = append(entries, line)
	}
	return entries, unlisted
}

func droppedEntries(dropped []contextkit.Message, assumeRefs bool) []string {
	results := map[string]contextkit.Message{}
	for _, msg := range dropped {
		if msg.Role == contextkit.RoleTool && msg.ToolCallID != "" {
			results[msg.ToolCallID] = msg
		}
	}
	answered := map[string]bool{}
	var entries []string
	for _, msg := range dropped {
		switch msg.Role {
		case contextkit.RoleUser, contextkit.RoleAssistant:
			if text := snippet(msg.Content, catalogSnippetRunes); text != "" {
				entries = append(entries, fmt.Sprintf("- %s: %q", msg.Role, text)+refSuffix(msg, assumeRefs))
			}
			for _, call := range msg.ToolCalls {
				entry := "- " + call.Name
				if args := snippet(call.Arguments, catalogArgsRunes); args != "" && args != "{}" {
					entry += " " + args
				}
				if result, ok := results[call.ID]; ok {
					answered[call.ID] = true
					entry += fmt.Sprintf(" → %d chars", resultChars(result)) + refSuffix(result, assumeRefs)
				}
				entries = append(entries, entry)
			}
		case contextkit.RoleTool:
			if !answered[msg.ToolCallID] {
				name := msg.ToolName
				if name == "" {
					name = "tool"
				}
				entries = append(entries, fmt.Sprintf("- %s result → %d chars", name, resultChars(msg))+refSuffix(msg, assumeRefs))
			}
		}
	}
	return entries
}

func refSuffix(msg contextkit.Message, assumeRefs bool) string {
	switch {
	case msg.Ref != "":
		return " ref=" + msg.Ref
	case assumeRefs:
		return " ref=" + assumedRef
	default:
		return ""
	}
}

func hasRef(entries []string) bool {
	for _, entry := range entries {
		if strings.Contains(entry, " ref=") {
			return true
		}
	}
	return false
}

func resultChars(msg contextkit.Message) int {
	switch n := msg.Metadata["contextkit.tool_original_chars"].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return len([]rune(msg.Content))
}

// snippet is text on one line, cut to limit runes.
func snippet(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
