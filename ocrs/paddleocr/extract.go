package paddleocr

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/eruca/goagents/ocrs"
)

var (
	citationRegex       = regexp.MustCompile(`\$\s*\^\{\[[\d\s,-]+\]\}\s*\$`)
	fixPunctuationRegex = regexp.MustCompile(`\s+([，。；：！？、])`)
	referenceRe         = regexp.MustCompile(`参\s*考\s*文\s*献`)
	htmlTagRegex        = regexp.MustCompile(`(?s)<[^>]*>`)
)

func ParseStructuredChunksByReader(rd io.Reader) (string, []ocrs.ProcessedChunk, error) {
	var data OCRResponse
	if err := json.NewDecoder(rd).Decode(&data); err != nil {
		return "", nil, err
	}
	return parseStructuredChunks(&data)
}

func ParseStructuredChunks(raw []byte) (string, []ocrs.ProcessedChunk, error) {
	var data OCRResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", nil, err
	}
	return parseStructuredChunks(&data)
}

func parseStructuredChunks(data *OCRResponse) (string, []ocrs.ProcessedChunk, error) {
	if data == nil {
		return "", nil, fmt.Errorf("nil ocr response")
	}
	if data.ErrorCode != 0 {
		return "", nil, fmt.Errorf("ocr error code=%d msg=%s", data.ErrorCode, data.ErrorMsg)
	}

	chunks := make([]ocrs.ProcessedChunk, 0)
	title := ""
	isChineseTitle := false
	globalChunkIdx := 0
	currentTitle := "Introduction"
	var currentBuffer strings.Builder

	flushChunk := func() {
		text := strings.TrimSpace(citationRegex.ReplaceAllString(currentBuffer.String(), ""))
		text = cleanChunkText(text)
		if text == "" || referenceRe.MatchString(currentTitle) {
			currentBuffer.Reset()
			return
		}

		titleText := cleanChunkText(currentTitle)
		fullContent := strings.TrimSpace(fmt.Sprintf("%s %s", titleText, text))
		chunks = append(chunks, ocrs.ProcessedChunk{
			Index: globalChunkIdx,
			Text:  fullContent,
		})

		globalChunkIdx++
		currentBuffer.Reset()
	}

	for _, page := range data.Result.LayoutParsingResults {
		for _, block := range page.PrunedResult.ParsingResList {
			content := strings.TrimSpace(block.BlockContent)
			if content == "" {
				continue
			}

			if block.BlockLabel == "doc_title" && !isChineseTitle {
				title = cleanTitleText(content)
				isChineseTitle = containsHan(content)
			}

			switch block.BlockLabel {
			case "doc_title", "paragraph_title", "abstract":
				flushChunk()
				currentTitle = content
			case "text", "table", "reference_content", "content", "figure_title", "table_title":
				currentBuffer.WriteString(content)
				currentBuffer.WriteString("\n")
			case "header", "footer", "number", "footnote", "vision_footnote", "aside_text":
				continue
			}
		}
	}

	flushChunk()
	return title, chunks, nil
}

func CleanTitleText(s string) string {
	return cleanTitleText(s)
}

func cleanTitleText(s string) string {
	if s == "" {
		return s
	}
	s = citationRegex.ReplaceAllString(s, "")
	s = htmlTagRegex.ReplaceAllString(s, " ")
	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := false
	for _, r := range s {
		switch r {
		case '（':
			r = '('
		case '）':
			r = ')'
		}
		if isAllowedTitleRune(r) {
			if unicode.IsSpace(r) {
				if lastWasSpace {
					continue
				}
				b.WriteByte(' ')
				lastWasSpace = true
				continue
			}
			b.WriteRune(r)
			lastWasSpace = false
			continue
		}
		if !lastWasSpace {
			b.WriteByte(' ')
			lastWasSpace = true
		}
	}
	return strings.Join(strings.Fields(strings.TrimSpace(b.String())), " ")
}

func isAllowedTitleRune(r rune) bool {
	if unicode.Is(unicode.Scripts["Han"], r) || unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	if unicode.IsSpace(r) {
		return true
	}
	return r == '(' || r == ')'
}

func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Scripts["Han"], r) {
			return true
		}
	}
	return false
}

func cleanChunkText(s string) string {
	if s == "" {
		return s
	}

	s = citationRegex.ReplaceAllString(s, "")
	s = htmlTagRegex.ReplaceAllString(s, " ")

	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := false
	for _, r := range s {
		if isAllowedRune(r) {
			if unicode.IsSpace(r) {
				if lastWasSpace {
					continue
				}
				b.WriteByte(' ')
				lastWasSpace = true
				continue
			}
			b.WriteRune(r)
			lastWasSpace = false
			continue
		}
		if utf8.ValidRune(r) {
			if !lastWasSpace {
				b.WriteByte(' ')
				lastWasSpace = true
			}
		}
	}

	out := strings.TrimSpace(b.String())
	out = fixPunctuationRegex.ReplaceAllString(out, "$1")
	out = collapseRepeatedPunctuation(out)
	out = strings.Join(strings.Fields(out), " ")
	return out
}

func isAllowedRune(r rune) bool {
	if unicode.Is(unicode.Scripts["Han"], r) || unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '%', '/', '-', '+', '.', ':', ';', ',', '，', '。', '；', '：', '！', '？', '、', '(', ')', '（', '）', '[', ']', '【', '】', '《', '》':
		return true
	default:
		return false
	}
}

func collapseRepeatedPunctuation(s string) string {
	if s == "" {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	var prev rune
	for i, r := range s {
		if i > 0 && r == prev && isCollapsiblePunctuation(r) {
			continue
		}
		out.WriteRune(r)
		prev = r
	}
	return out.String()
}

func isCollapsiblePunctuation(r rune) bool {
	switch r {
	case '，', '。', '；', '：', '！', '？', '、', '!', '?', ',', '.', ';', ':':
		return true
	default:
		return false
	}
}
