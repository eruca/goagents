package prompt

import (
	"context"
	"sort"
	"strings"
)

type Compiler struct{}

func NewCompiler() *Compiler {
	return &Compiler{}
}

func (c *Compiler) Compile(ctx context.Context, blocks []Block) (*Compiled, error) {
	ordered := append([]Block(nil), blocks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Mode != ordered[j].Mode {
			return modeRank(ordered[i].Mode) < modeRank(ordered[j].Mode)
		}
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].Name < ordered[j].Name
	})

	contents := make([]string, 0, len(ordered))
	for _, block := range ordered {
		if block.Content != "" {
			contents = append(contents, block.Content)
		}
	}

	return &Compiled{
		Blocks:  ordered,
		Content: strings.Join(contents, "\n"),
	}, nil
}

func modeRank(mode Mode) int {
	switch mode {
	case ModeCacheable:
		return 0
	default:
		return 1
	}
}
