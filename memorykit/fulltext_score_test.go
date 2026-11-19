package memorykit

import (
	"reflect"
	"testing"
	"time"
)

func TestFullTextWeightsFavorRareTerms(t *testing.T) {
	words, grams := FullTextWeights(FullTextStats{Documents: 100, WordDocuments: []int{1, 50, 100, 0}, GramDocuments: []int{10}})
	if !(words[0] > words[1] && words[1] > words[2] && words[2] >= 1) {
		t.Fatalf("weights not decreasing with document frequency: %v", words)
	}
	if words[3] != 0 {
		t.Fatalf("a term no memory contains weighs %d, want 0", words[3])
	}
	if grams[0] <= words[1] || grams[0] >= words[0] {
		t.Fatalf("gram weight %d not between %d and %d", grams[0], words[1], words[0])
	}
	if words, _ := FullTextWeights(FullTextStats{WordDocuments: []int{1}}); words[0] != 0 {
		t.Fatalf("weights without documents = %v", words)
	}
}

func TestRankFullTextCutoffLengthAndTies(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	stats := FullTextStats{Documents: 10, TotalLength: 100}
	matches := []FullTextMatch{
		{ID: "weak", WeightSum: 400, Length: 10},
		{ID: "long", WeightSum: 1000, Length: 40},
		{ID: "short", WeightSum: 1000, Length: 5},
		{ID: "tie-old", WeightSum: 1000, Length: 10, Importance: 5, UpdatedAt: now},
		{ID: "tie-new", WeightSum: 1000, Length: 10, Importance: 5, UpdatedAt: now.Add(time.Hour)},
		{ID: "tie-b", WeightSum: 1000, Length: 10},
		{ID: "tie-a", WeightSum: 1000, Length: 10},
	}
	ids := func(indexes []int) []string {
		out := make([]string, len(indexes))
		for i, index := range indexes {
			out[i] = matches[index].ID
		}
		return out
	}
	// "weak" scores under half of "short" and is cut; "long" is penalized
	// below the average-length matches but stays above the cutoff.
	want := []string{"short", "tie-new", "tie-old", "tie-a", "tie-b", "long"}
	if got := ids(RankFullText(matches, stats, 10)); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked = %v, want %v", got, want)
	}
	if got := ids(RankFullText(matches, stats, 2)); !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("limited = %v", got)
	}
	if RankFullText(nil, stats, 5) != nil || RankFullText(matches, FullTextStats{}, 5) != nil || RankFullText(matches, stats, 0) != nil {
		t.Fatal("empty input ranked something")
	}
}

func TestFullTextTermMatchesAndLength(t *testing.T) {
	words, grams := SearchTerms("storage 数据库")
	wordHits, gramHits := FullTextTermMatches("storage.vectors", "项目使用数据库", words, grams)
	if !reflect.DeepEqual(wordHits, []bool{true}) || !reflect.DeepEqual(gramHits, []bool{true, true}) {
		t.Fatalf("hits = %v %v", wordHits, gramHits)
	}
	// storage, vectors + 项,目,使,用,数,据,库 and their 6 bigrams.
	if got := FullTextLength("storage.vectors", "项目使用数据库"); got != 2+7+6 {
		t.Fatalf("length = %d", got)
	}
}
