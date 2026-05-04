package toolbudget

import "fmt"

type Config struct {
	MaxChars int
}

type Result struct {
	Content       string
	Truncated     bool
	OriginalChars int
	OmittedChars  int
}

func Apply(content string, cfg Config) Result {
	runes := []rune(content)
	out := Result{
		Content:       content,
		OriginalChars: len(runes),
	}
	if cfg.MaxChars <= 0 || len(runes) <= cfg.MaxChars {
		return out
	}
	out.Truncated = true
	out.OmittedChars = len(runes) - cfg.MaxChars
	out.Content = string(runes[:cfg.MaxChars]) + fmt.Sprintf("\n[truncated %d chars]", out.OmittedChars)
	return out
}
