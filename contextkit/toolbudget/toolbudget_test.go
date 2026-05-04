package toolbudget

import "testing"

func TestApplyKeepsShortContent(t *testing.T) {
	t.Parallel()

	got := Apply("short result", Config{MaxChars: 20})

	if got.Content != "short result" {
		t.Fatalf("unexpected content: %q", got.Content)
	}
	if got.Truncated {
		t.Fatalf("short content should not be truncated")
	}
	if got.OriginalChars != len("short result") {
		t.Fatalf("unexpected original chars: %d", got.OriginalChars)
	}
}

func TestApplyTruncatesLongContentWithMarker(t *testing.T) {
	t.Parallel()

	got := Apply("abcdefghijklmnopqrstuvwxyz", Config{MaxChars: 10})

	if !got.Truncated {
		t.Fatalf("expected truncation")
	}
	if got.OmittedChars != 16 {
		t.Fatalf("unexpected omitted chars: %d", got.OmittedChars)
	}
	if got.Content != "abcdefghij\n[truncated 16 chars]" {
		t.Fatalf("unexpected content: %q", got.Content)
	}
}
