package memorykit

import (
	"math"
	"strings"
	"unicode"
)

// Bounds on full-text query terms, so a pasted document cannot expand into an
// unbounded OR query.
const (
	maxSearchWords = 32
	maxSearchGrams = 64
)

// SearchTerms splits recall text into the terms every RecallStore matches the
// same way. Words are lowercase runs of letters and digits, as PostgreSQL's
// 'simple' text-search configuration produces them. Chinese and Japanese are
// written without spaces, so their runs become overlapping two-character grams
// (a lone character stays a unigram) that stores match as substrings.
func SearchTerms(text string) (words, grams []string) {
	return splitSearchTerms(text, maxSearchWords, maxSearchGrams)
}

func splitSearchTerms(text string, wordLimit, gramLimit int) (words, grams []string) {
	seenWords := map[string]struct{}{}
	seenGrams := map[string]struct{}{}
	add := func(terms *[]string, seen map[string]struct{}, limit int, term string) {
		if _, exists := seen[term]; exists || len(*terms) >= limit {
			return
		}
		seen[term] = struct{}{}
		*terms = append(*terms, term)
	}
	var word, cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			add(&words, seenWords, wordLimit, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	flushCJK := func() {
		if len(cjk) == 1 {
			add(&grams, seenGrams, gramLimit, string(cjk))
		}
		for i := 0; i+1 < len(cjk); i++ {
			add(&grams, seenGrams, gramLimit, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return words, grams
}

// CountSearchTermHits counts how many query terms a memory matches: words
// against the memory's own words, grams as substrings of its text.
func CountSearchTermHits(key, content string, words, grams []string) int {
	text := key + " " + content
	memoryWords, _ := splitSearchTerms(text, math.MaxInt, 0)
	present := make(map[string]struct{}, len(memoryWords))
	for _, word := range memoryWords {
		present[word] = struct{}{}
	}
	hits := 0
	for _, word := range words {
		if _, exists := present[word]; exists {
			hits++
		}
	}
	for _, gram := range grams {
		if strings.Contains(text, gram) {
			hits++
		}
	}
	return hits
}
