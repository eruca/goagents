package main

import (
	"encoding/json"
	"fmt"

	"github.com/eruca/ocrs/paddleocr"
)

func main() {
	raw, err := json.Marshal(paddleocr.OCRResponse{
		ErrorCode: 0,
		ErrorMsg:  "Success",
		Result: paddleocr.OCRResult{
			LayoutParsingResults: []paddleocr.LayoutParsingResult{
				{
					PrunedResult: paddleocr.PrunedResult{
						ParsingResList: []paddleocr.ParsingBlock{
							{BlockLabel: "doc_title", BlockContent: "OCR 示例文档"},
							{BlockLabel: "paragraph_title", BlockContent: "摘要"},
							{BlockLabel: "text", BlockContent: "这是一段 PaddleOCR 结构化结果。"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		panic(err)
	}

	title, chunks, err := paddleocr.ParseStructuredChunks(raw)
	if err != nil {
		panic(err)
	}
	fmt.Printf("title=%s chunks=%d\n", title, len(chunks))
	for _, chunk := range chunks {
		fmt.Printf("%d: %s\n", chunk.Index, chunk.Text)
	}
}
