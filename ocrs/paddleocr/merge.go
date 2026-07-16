package paddleocr

import (
	"encoding/json"
	"fmt"

	"github.com/eruca/goagents/ocrs"
)

func MergeChunkResponses(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
	var zero ocrs.OCRResult
	if len(results) == 0 {
		return zero, fmt.Errorf("no chunk results")
	}

	merged := OCRResponse{
		LogID:     "chunked",
		ErrorCode: 0,
		ErrorMsg:  "Success",
	}
	provider := results[0].Provider

	for i, r := range results {
		var parsed OCRResponse
		if err := json.Unmarshal(r.Raw, &parsed); err != nil {
			return zero, fmt.Errorf("decode chunk %d failed: %w", i, err)
		}
		if parsed.ErrorCode != 0 {
			return zero, fmt.Errorf("chunk %d api error code=%d msg=%s", i, parsed.ErrorCode, parsed.ErrorMsg)
		}

		merged.Result.LayoutParsingResults = append(merged.Result.LayoutParsingResults, parsed.Result.LayoutParsingResults...)
		merged.Result.PreprocessedImages = append(merged.Result.PreprocessedImages, parsed.Result.PreprocessedImages...)
		merged.Result.DataInfo.Pages = append(merged.Result.DataInfo.Pages, parsed.Result.DataInfo.Pages...)
		merged.Result.DataInfo.NumPages += parsed.Result.DataInfo.NumPages
		if merged.Result.DataInfo.Type == "" {
			merged.Result.DataInfo.Type = parsed.Result.DataInfo.Type
		}
	}

	raw, err := json.Marshal(merged)
	if err != nil {
		return zero, fmt.Errorf("encode merged response failed: %w", err)
	}
	return ocrs.OCRResult{
		Provider: provider,
		Raw:      raw,
	}, nil
}
