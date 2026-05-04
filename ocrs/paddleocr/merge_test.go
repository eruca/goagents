package paddleocr

import (
	"encoding/json"
	"testing"

	"github.com/eruca/ocrs"
)

func TestMergeChunkResponses(t *testing.T) {
	t.Parallel()

	r1 := OCRResponse{
		LogID:     "log-1",
		ErrorCode: 0,
		ErrorMsg:  "Success",
		Result: OCRResult{
			LayoutParsingResults: []LayoutParsingResult{
				{Markdown: MarkdownResult{Text: "p1"}},
			},
			DataInfo: DataInfo{
				NumPages: 1,
				Type:     "pdf",
				Pages: []PageInfo{
					{Width: 100, Height: 200},
				},
			},
		},
	}
	r2 := OCRResponse{
		LogID:     "log-2",
		ErrorCode: 0,
		ErrorMsg:  "Success",
		Result: OCRResult{
			LayoutParsingResults: []LayoutParsingResult{
				{Markdown: MarkdownResult{Text: "p2"}},
			},
			DataInfo: DataInfo{
				NumPages: 1,
				Type:     "pdf",
				Pages: []PageInfo{
					{Width: 300, Height: 400},
				},
			},
		},
	}

	raw1, _ := json.Marshal(r1)
	raw2, _ := json.Marshal(r2)

	merged, err := MergeChunkResponses([]ocrs.OCRResult{
		{Provider: "paddleocr", Raw: raw1},
		{Provider: "paddleocr", Raw: raw2},
	})
	if err != nil {
		t.Fatalf("MergeChunkResponses() error = %v", err)
	}

	var got OCRResponse
	if err := json.Unmarshal(merged.Raw, &got); err != nil {
		t.Fatalf("unmarshal merged raw failed: %v", err)
	}
	if got.Result.DataInfo.NumPages != 2 {
		t.Fatalf("unexpected NumPages: got=%d want=2", got.Result.DataInfo.NumPages)
	}
	if len(got.Result.DataInfo.Pages) != 2 {
		t.Fatalf("unexpected pages len: got=%d want=2", len(got.Result.DataInfo.Pages))
	}
	if len(got.Result.LayoutParsingResults) != 2 {
		t.Fatalf("unexpected layout results len: got=%d want=2", len(got.Result.LayoutParsingResults))
	}
	if got.Result.LayoutParsingResults[0].Markdown.Text != "p1" || got.Result.LayoutParsingResults[1].Markdown.Text != "p2" {
		t.Fatalf("unexpected merge order")
	}
}
