package memorykit

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Full-text ranking is a BM25 variant for short memories. Each query term
// weighs its inverse document frequency, so a term most memories share counts
// little and a rare one counts much. Term frequency is ignored: a memory is a
// sentence or two and rarely repeats a term. Length is normalized, so a long
// memory does not outrank a short one only because it contains more terms.
//
// Stores compute only integers (statistics, weight sums, lengths) and leave
// scoring, cutoff, and order to RankFullText, so every store ranks alike.
const (
	fullTextK1 = 1.2
	fullTextB  = 0.75
	// IDF weights are fixed-point integers so that summing them is exact in
	// any store and in any order.
	fullTextWeightScale = 1000
)

// FullTextStats describes the memories a full-text query ranks against: the
// active, currently effective memories of the query's Scope, whatever kinds
// the query asks for.
type FullTextStats struct {
	// Documents is the number of such memories.
	Documents int
	// TotalLength sums FullTextLength over them.
	TotalLength int
	// WordDocuments and GramDocuments count, for each query word and gram in
	// SearchTerms order, how many of them contain it.
	WordDocuments, GramDocuments []int
}

// FullTextLength is a memory's length for normalization: the number of
// distinct words and grams IndexWords and IndexGrams find in its key and
// content.
func FullTextLength(key, content string) int {
	text := key + " " + content
	return len(IndexWords(text)) + len(IndexGrams(text))
}

// FullTextWeights returns each query word's and gram's IDF weight, in the
// order of stats.WordDocuments and stats.GramDocuments. A store sums the
// weights of the terms a memory matches into the memory's weight sum.
func FullTextWeights(stats FullTextStats) (words, grams []int64) {
	weigh := func(documents []int) []int64 {
		weights := make([]int64, len(documents))
		for index, df := range documents {
			if df <= 0 || stats.Documents <= 0 {
				continue
			}
			n, d := float64(stats.Documents), float64(df)
			idf := math.Log(1 + (n-d+0.5)/(d+0.5))
			weights[index] = max(1, int64(math.Round(idf*fullTextWeightScale)))
		}
		return weights
	}
	return weigh(stats.WordDocuments), weigh(stats.GramDocuments)
}

// FullTextMatch is one memory that matched at least one query term.
type FullTextMatch struct {
	ID         string
	WeightSum  int64
	Length     int
	Importance int
	UpdatedAt  time.Time
}

// RankFullText keeps the matches whose weight sum is more than half the best
// weight sum, so a memory sharing only common terms with the query is dropped,
// and returns the indexes of at most limit of them, best score first. Length
// affects only the order: a long memory that matches the same terms as a short
// one ranks after it but is not dropped. Ties rank by importance, then most
// recent update, then ID.
func RankFullText(matches []FullTextMatch, stats FullTextStats, limit int) []int {
	if len(matches) == 0 || limit <= 0 || stats.Documents <= 0 {
		return nil
	}
	averageLength := float64(stats.TotalLength) / float64(stats.Documents)
	if averageLength <= 0 {
		averageLength = 1
	}
	scores := make([]float64, len(matches))
	var bestWeight int64
	for index, match := range matches {
		scores[index] = fullTextScore(match.WeightSum, match.Length, averageLength)
		bestWeight = max(bestWeight, match.WeightSum)
	}
	kept := make([]int, 0, len(matches))
	for index, match := range matches {
		if match.WeightSum > 0 && match.WeightSum*2 > bestWeight {
			kept = append(kept, index)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		left, right := matches[kept[i]], matches[kept[j]]
		if scores[kept[i]] != scores[kept[j]] {
			return scores[kept[i]] > scores[kept[j]]
		}
		if left.Importance != right.Importance {
			return left.Importance > right.Importance
		}
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return strings.Compare(left.ID, right.ID) < 0
	})
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return kept
}

func fullTextScore(weightSum int64, length int, averageLength float64) float64 {
	if weightSum <= 0 {
		return 0
	}
	normalized := float64(1 - fullTextB + fullTextB*float64(length)/averageLength)
	return float64(weightSum) * (fullTextK1 + 1) / float64(1+fullTextK1*normalized)
}

// FullTextTermMatches reports which query words and grams a memory contains,
// by the rule CountSearchTermHits counts: words against the memory's own
// words, grams as substrings of its key and content.
func FullTextTermMatches(key, content string, words, grams []string) (wordHits, gramHits []bool) {
	text := key + " " + content
	present := make(map[string]struct{})
	for _, word := range IndexWords(text) {
		present[word] = struct{}{}
	}
	wordHits = make([]bool, len(words))
	for index, word := range words {
		_, wordHits[index] = present[word]
	}
	gramHits = make([]bool, len(grams))
	for index, gram := range grams {
		gramHits[index] = strings.Contains(text, gram)
	}
	return wordHits, gramHits
}
