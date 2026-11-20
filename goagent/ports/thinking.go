package ports

import "strings"

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// SplitThinkTags separates reasoning that a model writes into its content
// with <think> tags, as Qwen3 and DeepSeek-R1 do when served without a
// reasoning parser. It recognizes:
//
//   - content that starts, after whitespace, with a <think>...</think> block;
//   - content whose first </think> has no <think> before it, because the chat
//     template already opened the block in the prompt.
//
// It returns the reasoning as written between the tags and the answer
// without its leading whitespace, so a streaming client can emit both as
// they arrive and still match. Content without either form is returned
// unchanged as the answer. An unclosed <think> block is all reasoning: the
// model stopped before answering.
func SplitThinkTags(content string) (reasoning, answer string) {
	trimmed := strings.TrimLeft(content, " \t\r\n")
	if strings.HasPrefix(trimmed, thinkOpenTag) {
		body := trimmed[len(thinkOpenTag):]
		end := strings.Index(body, thinkCloseTag)
		if end < 0 {
			return body, ""
		}
		return body[:end], trimLeadingSpace(body[end+len(thinkCloseTag):])
	}
	end := strings.Index(content, thinkCloseTag)
	if end >= 0 && !strings.Contains(content[:end], thinkOpenTag) {
		return content[:end], trimLeadingSpace(content[end+len(thinkCloseTag):])
	}
	return "", content
}

func trimLeadingSpace(text string) string {
	return strings.TrimLeft(text, " \t\r\n")
}
