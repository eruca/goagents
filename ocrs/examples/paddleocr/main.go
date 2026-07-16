package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/eruca/goagents/ocrs/paddleocr"
)

func main() {
	apiURL := os.Getenv("PADDLEOCR_API_URL")
	token := os.Getenv("PADDLEOCR_TOKEN")
	filePath := os.Getenv("OCR_FILE")
	if apiURL == "" || token == "" || filePath == "" {
		fmt.Println("skip: set PADDLEOCR_API_URL, PADDLEOCR_TOKEN, and OCR_FILE to run this example")
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		panic(err)
	}

	client := paddleocr.NewClient("paddleocr", apiURL, token, 10*time.Minute)
	result, err := client.Handle(context.Background(), data)
	if err != nil {
		panic(err)
	}

	title, chunks, err := paddleocr.ParseStructuredChunks(result.Raw)
	if err != nil {
		panic(err)
	}
	fmt.Printf("provider=%s title=%s chunks=%d\n", result.Provider, title, len(chunks))
}
