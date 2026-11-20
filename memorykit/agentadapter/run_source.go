package agentadapter

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/eruca/goagents/goagent/agentcore"
)

// minToolEntryRunes is the smallest share a tool call gets in a run source;
// with less room, the oldest calls are left out instead.
const minToolEntryRunes = 120

// RunSource renders a finished run's messages (agentcore.RunResult.Messages)
// as extraction source text of at most maxRunes runes: the request, each tool
// call with the start of its result, and the final answer. Facts a run found
// through tools then reach memory extraction, not only its answer.
//
// When the text does not fit, the request and the answer keep up to a fifth
// and two fifths of the room, and tool results share the rest equally; when
// even that leaves too little per call, the oldest calls are left out.
// Reasoning and assistant text between tool calls are left out: they are the
// model's own guesses, not what it found.
func RunSource(messages []agentcore.Message, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	var requests []string
	var entries []toolEntry
	byID := map[string]int{}
	answer := ""
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			if strings.TrimSpace(msg.Content) != "" {
				requests = append(requests, msg.Content)
			}
		case "assistant":
			if len(msg.ToolCalls) == 0 {
				answer = msg.Content
				continue
			}
			for _, call := range msg.ToolCalls {
				byID[call.ID] = len(entries)
				entries = append(entries, toolEntry{call: strings.TrimSpace(call.Name + " " + string(call.Input))})
			}
		case "tool":
			if index, ok := byID[msg.ToolCallID]; ok {
				entries[index].result = msg.Content
			}
		}
	}
	request := strings.Join(requests, "\n")

	const (
		requestHeader = "Request:\n"
		toolsHeader   = "\n\nTool calls:\n"
		answerHeader  = "\n\nAnswer:\n"
	)
	room := maxRunes - runeLen(requestHeader) - runeLen(answerHeader)
	if len(entries) > 0 {
		room -= runeLen(toolsHeader)
	}
	if room <= 0 {
		return clip(requestHeader+request, maxRunes)
	}
	// The request and answer take what they need up to their shares; tool
	// results get the rest, so short parts leave more room for results.
	request = clip(request, max(room/5, room-runeLen(answer)-toolsNeed(entries)))
	answer = clip(answer, max(room*2/5, room-runeLen(request)-toolsNeed(entries)))
	toolRoom := room - runeLen(request) - runeLen(answer)

	var lines []string
	if len(entries) > 0 && toolRoom > 0 {
		keep := len(entries)
		if toolRoom/keep < minToolEntryRunes {
			keep = max(1, toolRoom/minToolEntryRunes)
		}
		omitted := len(entries) - keep
		if omitted > 0 {
			note := fmt.Sprintf("- (%d earlier tool calls left out)", omitted)
			lines = append(lines, note)
			toolRoom -= runeLen(note) + 1
		}
		kept := entries[omitted:]
		for i, entry := range kept {
			share := toolRoom / (len(kept) - i)
			line := clip("- "+entry.call+" → "+strings.TrimSpace(entry.result), max(share-1, 0))
			toolRoom -= runeLen(line) + 1
			lines = append(lines, line)
		}
	}

	var out strings.Builder
	out.WriteString(requestHeader + request)
	if len(lines) > 0 {
		out.WriteString(toolsHeader + strings.Join(lines, "\n"))
	}
	out.WriteString(answerHeader + answer)
	return clip(out.String(), maxRunes)
}

type toolEntry struct{ call, result string }

func toolsNeed(entries []toolEntry) int {
	need := 0
	for _, entry := range entries {
		need += runeLen(entry.call) + runeLen(entry.result) + 6
	}
	return need
}

func runeLen(text string) int { return utf8.RuneCountInString(text) }

// clip shortens text to at most limit runes, marking the cut.
func clip(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if runeLen(text) <= limit {
		return text
	}
	const marker = " …"
	if limit <= runeLen(marker) {
		return string([]rune(text)[:limit])
	}
	return string([]rune(text)[:limit-runeLen(marker)]) + marker
}
