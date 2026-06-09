package skillroute

import (
	"bytes"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

type bm25Index struct {
	docs         map[string]bm25Doc
	documentFreq map[string]int
	avgDocLen    float64
}

type bm25Doc struct {
	length int
	tf     map[string]int
}

func (c *Corpus) Route(query string, limit int) []Selection {
	if c == nil || limit <= 0 {
		return nil
	}
	queryTokens := uniqueTokens(tokenize(query))
	if len(queryTokens) == 0 || len(c.skillIDs) == 0 {
		return nil
	}

	selections := make([]Selection, 0, len(c.skillIDs))
	for _, id := range c.skillIDs {
		doc := c.index.docs[id]
		score := c.index.score(doc, len(c.skillIDs), queryTokens)
		if score > 0 && !math.IsNaN(score) && !math.IsInf(score, 0) {
			selections = append(selections, Selection{ID: id, Score: score})
		}
	}
	if len(selections) == 0 {
		return nil
	}
	sort.Slice(selections, func(i, j int) bool {
		if selections[i].Score == selections[j].Score {
			return selections[i].ID < selections[j].ID
		}
		return selections[i].Score > selections[j].Score
	})
	if limit > len(selections) {
		limit = len(selections)
	}
	return selections[:limit]
}

func newBM25Index(ids []string, skills map[string]*Skill) bm25Index {
	index := bm25Index{
		docs:         map[string]bm25Doc{},
		documentFreq: map[string]int{},
	}
	if len(ids) == 0 {
		return index
	}

	totalLen := 0
	for _, id := range ids {
		skill := skills[id]
		doc := bm25Doc{
			length: skill.tokenLength,
			tf:     cloneTermFreq(skill.tokenFreq),
		}
		for tok := range doc.tf {
			index.documentFreq[tok]++
		}
		totalLen += doc.length
		index.docs[id] = doc
	}
	index.avgDocLen = float64(totalLen) / float64(len(ids))
	return index
}

func (idx bm25Index) score(doc bm25Doc, docCount int, queryTokens []string) float64 {
	if doc.length == 0 || docCount == 0 || idx.avgDocLen == 0 {
		return 0
	}

	var score float64
	for _, tok := range queryTokens {
		tf := doc.tf[tok]
		if tf == 0 {
			continue
		}
		df := idx.documentFreq[tok]
		idf := math.Log(1 + (float64(docCount)-float64(df)+0.5)/(float64(df)+0.5))
		freq := float64(tf)
		denom := freq + bm25K1*(1-bm25B+bm25B*float64(doc.length)/idx.avgDocLen)
		if denom == 0 {
			continue
		}
		score += idf * (freq * (bm25K1 + 1) / denom)
	}
	return score
}

func weightedTerms(files []File) (map[string]int, int) {
	terms := map[string]int{}
	length := 0
	for _, f := range files {
		addWeightedTokens(terms, &length, tokenize(f.Path), PathWeight)
		if isSkillMD(f.Path) {
			addWeightedTokens(terms, &length, tokenize(string(f.Content)), SkillMDWeight)
			continue
		}
		if textLike(f.Content) {
			addWeightedTokens(terms, &length, tokenize(string(f.Content)), TextFileWeight)
		}
	}
	return terms, length
}

func addWeightedTokens(terms map[string]int, length *int, tokens []string, weight int) {
	if weight <= 0 {
		return
	}
	for _, tok := range tokens {
		terms[tok] += weight
		*length += weight
	}
}

func sortedTermKeys(terms map[string]int) []string {
	keys := make([]string, 0, len(terms))
	for tok := range terms {
		keys = append(keys, tok)
	}
	sort.Strings(keys)
	return keys
}

func cloneTermFreq(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for tok, n := range in {
		out[tok] = n
	}
	return out
}

func tokenize(s string) []string {
	var tokens []string
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		tokens = append(tokens, b.String())
	}
	return tokens
}

func uniqueTokens(tokens []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

func isSkillMD(p string) bool {
	_, rest, ok := strings.Cut(p, "/")
	return ok && rest == "SKILL.md"
}

func textLike(content []byte) bool {
	if !utf8.Valid(content) || bytes.Contains(content, []byte{0}) {
		return false
	}
	if len(content) == 0 {
		return true
	}

	var good, total int
	for len(content) > 0 {
		r, size := utf8.DecodeRune(content)
		content = content[size:]
		total++
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			good++
		}
	}
	return good*100 >= total*TextLikePercent
}
