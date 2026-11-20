package ports

import "testing"

func TestSplitThinkTags(t *testing.T) {
	cases := []struct {
		name, content, reasoning, answer string
	}{
		{"no tags", "plain answer", "", "plain answer"},
		{"leading block", "<think>\nweigh options\n</think>\n\nanswer", "\nweigh options\n", "answer"},
		{"leading whitespace", "  \n<think>a</think>b", "a", "b"},
		{"template opened the block", "weigh options\n</think>\nanswer\n", "weigh options\n", "answer\n"},
		{"unclosed block", "<think>still thinking", "still thinking", ""},
		{"empty block", "<think></think>answer", "", "answer"},
		{"tags inside the answer are kept", "Use the <think> tag like <think>x</think>.", "", "Use the <think> tag like <think>x</think>."},
		{"only the first block is reasoning", "<think>a</think>b<think>c</think>", "a", "b<think>c</think>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reasoning, answer := SplitThinkTags(tc.content)
			if reasoning != tc.reasoning || answer != tc.answer {
				t.Fatalf("SplitThinkTags(%q) = %q, %q; want %q, %q", tc.content, reasoning, answer, tc.reasoning, tc.answer)
			}
		})
	}
}

func TestResponseDeltasCarryReasoningSeparately(t *testing.T) {
	deltas := ResponseDeltas(&ChatResponse{Reasoning: "r", Content: "c"})
	if len(deltas) != 2 || deltas[0].Reasoning != "r" || deltas[0].Content != "" || deltas[1].Content != "c" {
		t.Fatalf("deltas = %+v", deltas)
	}
}
