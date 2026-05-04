package paddleocr

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseStructuredChunks(t *testing.T) {
	t.Parallel()

	sample := OCRResponse{
		ErrorCode: 0,
		ErrorMsg:  "Success",
		Result: OCRResult{
			LayoutParsingResults: []LayoutParsingResult{
				{
					PrunedResult: PrunedResult{
						ParsingResList: []ParsingBlock{
							{BlockLabel: "doc_title", BlockContent: "心肌梗死诊疗进展"},
							{BlockLabel: "text", BlockContent: "这是第一段 $^{[1, 2]}$ 。"},
							{BlockLabel: "header", BlockContent: "页眉噪音"},
							{BlockLabel: "paragraph_title", BlockContent: "方法"},
							{BlockLabel: "text", BlockContent: "这是第二段 ，包含流程。"},
							{BlockLabel: "paragraph_title", BlockContent: "参考文献"},
							{BlockLabel: "reference_content", BlockContent: "[1] xxx"},
						},
					},
				},
			},
		},
	}

	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("marshal sample failed: %v", err)
	}

	title, chunks, err := ParseStructuredChunks(raw)
	if err != nil {
		t.Fatalf("ParseStructuredChunks() error = %v", err)
	}

	if title != "心肌梗死诊疗进展" {
		t.Fatalf("unexpected title: %s", title)
	}
	if len(chunks) != 2 {
		t.Fatalf("unexpected chunks len: got=%d want=2", len(chunks))
	}
	if chunks[0].Text != "心肌梗死诊疗进展 这是第一段。" {
		t.Fatalf("unexpected chunk[0]: %q", chunks[0].Text)
	}
	if chunks[1].Text != "方法 这是第二段，包含流程。" {
		t.Fatalf("unexpected chunk[1]: %q", chunks[1].Text)
	}
	if chunks[0].Index != 0 || chunks[1].Index != 1 {
		t.Fatalf("unexpected chunk index sequence")
	}
}

func TestParseStructuredChunksTitleRemovesPunctuationExceptParentheses(t *testing.T) {
	t.Parallel()

	sample := OCRResponse{
		ErrorCode: 0,
		ErrorMsg:  "Success",
		Result: OCRResult{
			LayoutParsingResults: []LayoutParsingResult{
				{
					PrunedResult: PrunedResult{
						ParsingResList: []ParsingBlock{
							{BlockLabel: "doc_title", BlockContent: "中国扩张型心肌病诊断和治疗指南 $ ^{*} $（2024版）"},
							{BlockLabel: "text", BlockContent: "正文"},
						},
					},
				},
			},
		},
	}

	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("marshal sample failed: %v", err)
	}
	title, _, err := ParseStructuredChunks(raw)
	if err != nil {
		t.Fatalf("ParseStructuredChunks() error = %v", err)
	}
	if title != "中国扩张型心肌病诊断和治疗指南 (2024版)" {
		t.Fatalf("unexpected cleaned title: %s", title)
	}
}

func TestParseStructuredChunksCleansHTMLAndNoiseChars(t *testing.T) {
	t.Parallel()

	sample := OCRResponse{
		ErrorCode: 0,
		ErrorMsg:  "Success",
		Result: OCRResult{
			LayoutParsingResults: []LayoutParsingResult{
				{
					PrunedResult: PrunedResult{
						ParsingResList: []ParsingBlock{
							{BlockLabel: "paragraph_title", BlockContent: "方法"},
							{BlockLabel: "text", BlockContent: "<div>cIAI 治疗 😀 \u0007</div> ！！！   ???"},
							{BlockLabel: "text", BlockContent: "   保留 70% / 12-24 h ;;;"},
						},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("marshal sample failed: %v", err)
	}

	_, chunks, err := ParseStructuredChunks(raw)
	if err != nil {
		t.Fatalf("ParseStructuredChunks() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("unexpected chunks len: %d", len(chunks))
	}

	got := chunks[0].Text
	if strings.Contains(got, "<div>") || strings.Contains(got, "</div>") {
		t.Fatalf("html tag should be removed: %q", got)
	}
	if strings.Contains(got, "😀") {
		t.Fatalf("emoji should be removed: %q", got)
	}
	if strings.Contains(got, "???") || strings.Contains(got, ";;;") || strings.Contains(got, "！！！") {
		t.Fatalf("duplicated punctuation should be compressed: %q", got)
	}
	if !strings.Contains(got, "70% / 12-24 h") {
		t.Fatalf("medical expression should be preserved: %q", got)
	}
}
