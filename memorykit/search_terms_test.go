package memorykit

import (
	"reflect"
	"strings"
	"testing"
)

func TestSearchTermsSplitsWordsAndCJKGrams(t *testing.T) {
	words, grams := SearchTerms("Which DB did we pick? 我们选了哪个数据库，PostgreSQL_v17 还是 库")
	if !reflect.DeepEqual(words, []string{"which", "db", "did", "we", "pick", "postgresql", "v17"}) {
		t.Fatalf("words = %q", words)
	}
	wantGrams := []string{"我们", "们选", "选了", "了哪", "哪个", "个数", "数据", "据库", "还是", "库"}
	if !reflect.DeepEqual(grams, wantGrams) {
		t.Fatalf("grams = %q, want %q", grams, wantGrams)
	}
}

func TestSearchTermsDeduplicatesAndBoundsTerms(t *testing.T) {
	words, grams := SearchTerms(strings.Repeat("alpha beta ", 10) + strings.Repeat("一二三四五六七八九十", 20))
	if !reflect.DeepEqual(words, []string{"alpha", "beta"}) {
		t.Fatalf("words = %q", words)
	}
	if len(grams) != 10 {
		t.Fatalf("grams = %d, want 10 distinct", len(grams))
	}
	many := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		many = append(many, "w"+strings.Repeat("x", i+1))
	}
	words, _ = SearchTerms(strings.Join(many, " "))
	if len(words) != maxSearchWords {
		t.Fatalf("words = %d, want cap %d", len(words), maxSearchWords)
	}
}

func TestCountSearchTermHitsSeesEveryWordOfLongMemory(t *testing.T) {
	content := strings.Repeat("filler ", 100) + "pgvector"
	if hits := CountSearchTermHits("key", content, []string{"pgvector", "absent"}, nil); hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
	if hits := CountSearchTermHits("storage.database", "项目使用数据库", nil, []string{"数据", "据库", "天气"}); hits != 2 {
		t.Fatalf("gram hits = %d, want 2", hits)
	}
}
