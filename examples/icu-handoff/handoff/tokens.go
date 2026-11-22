package handoff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// refPattern is the gocms reference shape: a type letter, a row id and two
// lowercase check letters (for example L8812345dk).
var refPattern = regexp.MustCompile(`^[A-Z][1-9][0-9]{0,17}[a-z]{2}$`)

// refScan finds reference candidates inside text; boundaries are checked by
// hand because Go regexps have no look-behind.
var refScan = regexp.MustCompile(`[A-Z][1-9][0-9]{0,17}[a-z]{2}`)

// IsRef reports whether s has the gocms reference shape.
func IsRef(s string) bool { return refPattern.MatchString(s) }

// ExtractRefs returns the gocms references that appear in text, in order of
// first appearance. A candidate counts only when it is not glued to other
// ASCII letters or digits, so words such as "SpO2" never yield one.
func ExtractRefs(text string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, loc := range refSpans(text) {
		ref := text[loc[0]:loc[1]]
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}

func refSpans(text string) [][]int {
	var spans [][]int
	for _, loc := range refScan.FindAllStringIndex(text, -1) {
		if loc[0] > 0 && isASCIIAlnum(text[loc[0]-1]) {
			continue
		}
		if loc[1] < len(text) && isASCIIAlnum(text[loc[1]]) {
			continue
		}
		spans = append(spans, loc)
	}
	return spans
}

// stripRefs blanks out references so their row ids are not read as numbers.
func stripRefs(text string) string {
	spans := refSpans(text)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, loc := range spans {
		b.WriteString(text[last:loc[0]])
		b.WriteByte(' ')
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func isASCIIAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// TokenKind is what a numeric token in text stands for.
type TokenKind string

const (
	// TokenNumber is a plain number: integer or decimal, with its minus sign
	// when the sign is not a separator ("RASS -2", but not "11-22").
	TokenNumber TokenKind = "number"
	// TokenDate is a calendar date, normalised to "MM-DD".
	TokenDate TokenKind = "date"
	// TokenTime is a clock time, normalised to "HH:mm".
	TokenTime TokenKind = "time"
	// TokenPair is "a-b" with an ASCII hyphen where a and b could be a month
	// and a day: either a date or a range written with a hyphen.
	TokenPair TokenKind = "pair"
)

// Token is one numeric token found in text.
type Token struct {
	Kind  TokenKind
	Value string // normalised: "3.2", "-2", "11-21", "05:40", "3-5"
	A, B  string // the two numbers of a TokenPair
}

func (t Token) String() string { return t.Value }

// numericScan lists the token shapes in priority order; Go's alternation is
// leftmost-first, so at one position the earlier shape wins.
var numericScan = regexp.MustCompile(
	`(\d{4})[-/年](\d{1,2})[-/月](\d{1,2})日?` + // 1-3: full date
		`|(\d{1,2})月(\d{1,2})日` + // 4-5: 11月21日
		`|(\d{1,2}):(\d{2})(?::\d{2})?` + // 6-7: 05:40[:00]
		`|(\d{1,2})-(\d{1,2})(?:[^\d.:]|$)` + // 8-9: 11-21 or 3-5
		`|([-+]?)(\d+(?:\.\d+)?)`, // 10-11: number with optional sign
)

// fullWidth maps full-width digits and the punctuation used around numbers
// to ASCII, so "３．２" and "3.2" compare equal.
var fullWidth = strings.NewReplacer(
	"０", "0", "１", "1", "２", "2", "３", "3", "４", "4",
	"５", "5", "６", "6", "７", "7", "８", "8", "９", "9",
	"．", ".", "－", "-", "＋", "+", "／", "/", "−", "-",
)

// Tokens returns the numeric tokens of text. The rules:
//
//   - gocms references (L8812345dk) are removed first;
//   - a number glued to a preceding ASCII letter is part of a name and is
//     skipped ("SpO2", "FiO2", "q8h", "IL-6");
//   - dates ("2026-11-21", "11-21", "11月21日") and clock times ("05:40") are
//     single tokens, compared whole;
//   - "a-b" with an ASCII hyphen that could be a month-day is a pair: it
//     passes as a date or when both numbers pass;
//   - a leading "+" is dropped; a "-" is a minus sign only when it follows
//     neither a letter nor a digit;
//   - everything else between numbers splits them: "300/0.5" is 300 and 0.5,
//     "62–82" (en dash) is 62 and 82.
func Tokens(text string) []Token {
	text = stripRefs(fullWidth.Replace(text))
	var out []Token
	for _, m := range numericScan.FindAllStringSubmatchIndex(text, -1) {
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return text[m[2*i]:m[2*i+1]]
		}
		start := m[0]
		if start > 0 && isASCIILetter(text[start-1]) {
			continue
		}
		switch {
		case m[2] >= 0:
			out = append(out, dateToken(group(2), group(3)))
		case m[8] >= 0:
			out = append(out, dateToken(group(4), group(5)))
		case m[12] >= 0:
			out = append(out, timeToken(group(6), group(7)))
		case m[16] >= 0:
			a, b := group(8), group(9)
			if validMonthDay(a, b) {
				out = append(out, Token{Kind: TokenPair, Value: a + "-" + b, A: a, B: b})
			} else {
				out = append(out, Token{Kind: TokenNumber, Value: a}, Token{Kind: TokenNumber, Value: b})
			}
		default:
			sign, number := group(10), group(11)
			if sign == "-" && start > 0 && isASCIIAlnum(text[start-1]) {
				sign = ""
			}
			if sign == "+" {
				sign = ""
			}
			out = append(out, Token{Kind: TokenNumber, Value: sign + number})
		}
	}
	return out
}

func dateToken(month, day string) Token {
	return Token{Kind: TokenDate, Value: pad2(month) + "-" + pad2(day)}
}

func timeToken(hour, minute string) Token {
	return Token{Kind: TokenTime, Value: pad2(hour) + ":" + minute}
}

func pad2(s string) string {
	n, err := strconv.Atoi(s)
	if err != nil {
		return s
	}
	return fmt.Sprintf("%02d", n)
}

func validMonthDay(month, day string) bool {
	m, err1 := strconv.Atoi(month)
	d, err2 := strconv.Atoi(day)
	return err1 == nil && err2 == nil && m >= 1 && m <= 12 && d >= 1 && d <= 31
}

// Evidence is everything the tools returned for one patient in this handoff:
// the prefetched overview and every tool result the model saw. Points are
// checked against it.
type Evidence struct {
	mu      sync.Mutex
	chars   int
	refs    map[string]struct{}
	numbers map[string]struct{}
	dates   map[string]struct{}
	times   map[string]struct{}
}

// NewEvidence returns an empty Evidence.
func NewEvidence() *Evidence {
	return &Evidence{
		refs:    map[string]struct{}{},
		numbers: map[string]struct{}{},
		dates:   map[string]struct{}{},
		times:   map[string]struct{}{},
	}
}

// Add records one tool return text. It is safe for concurrent use.
func (e *Evidence) Add(text string) {
	refs := ExtractRefs(text)
	tokens := Tokens(text)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.chars += len([]rune(text))
	for _, ref := range refs {
		e.refs[ref] = struct{}{}
	}
	for _, token := range tokens {
		switch token.Kind {
		case TokenNumber:
			e.numbers[token.Value] = struct{}{}
		case TokenDate:
			e.dates[token.Value] = struct{}{}
		case TokenTime:
			e.times[token.Value] = struct{}{}
		case TokenPair:
			// In tool text a hyphenated pair is a date ("11-21"); its parts
			// also count as numbers so "3-5" ranges match either way.
			e.dates[pad2(token.A)+"-"+pad2(token.B)] = struct{}{}
			e.numbers[token.A] = struct{}{}
			e.numbers[token.B] = struct{}{}
		}
	}
}

// HasRef reports whether ref appeared in any recorded tool return.
func (e *Evidence) HasRef(ref string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.refs[ref]
	return ok
}

// Chars is the total length of the recorded tool returns in characters.
func (e *Evidence) Chars() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.chars
}

// Missing returns the tokens of text that do not appear in the evidence.
func (e *Evidence) Missing(text string) (tokens []Token, total int) {
	all := Tokens(text)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, token := range all {
		if !e.hasToken(token) {
			tokens = append(tokens, token)
		}
	}
	return tokens, len(all)
}

func (e *Evidence) hasToken(token Token) bool {
	switch token.Kind {
	case TokenNumber:
		_, ok := e.numbers[token.Value]
		return ok
	case TokenDate:
		_, ok := e.dates[token.Value]
		return ok
	case TokenTime:
		_, ok := e.times[token.Value]
		return ok
	case TokenPair:
		if _, ok := e.dates[pad2(token.A)+"-"+pad2(token.B)]; ok {
			return true
		}
		_, a := e.numbers[token.A]
		_, b := e.numbers[token.B]
		return a && b
	}
	return false
}
