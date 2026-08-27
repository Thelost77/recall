package search

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/chunk"
	"github.com/Thelost77/recall/internal/embed"
	"github.com/Thelost77/recall/internal/store"
)

const (
	DefaultLimit   = 3
	candidateLimit = 100
	rrfK           = 60.0
)

type Filters struct {
	Harnesses []string
	Path      string
	Roles     []string
	Since     time.Time
	Before    time.Time
	Limit     int
}

type ChannelRanks struct {
	Exact    int `json:"exact,omitempty"`
	Lexical  int `json:"lexical,omitempty"`
	Fuzzy    int `json:"fuzzy,omitempty"`
	Semantic int `json:"semantic,omitempty"`
}

type Result struct {
	Harness            string       `json:"harness"`
	SessionID          string       `json:"sessionId"`
	ParentID           string       `json:"parentId,omitempty"`
	Name               string       `json:"name"`
	CWD                string       `json:"cwd,omitempty"`
	StartedAt          time.Time    `json:"startedAt,omitempty"`
	UpdatedAt          time.Time    `json:"updatedAt,omitempty"`
	SourcePath         string       `json:"sourcePath"`
	EntryID            string       `json:"entryId,omitempty"`
	Excerpt            string       `json:"excerpt,omitempty"`
	Resume             string       `json:"resume"`
	Ranks              ChannelRanks `json:"ranks,omitempty"`
	SemanticSimilarity float64      `json:"semanticSimilarity,omitempty"`
	CombinedRank       int          `json:"rank"`
}

type Response struct {
	Results       []Result `json:"results"`
	SemanticError string   `json:"semanticError,omitempty"`
}

type Searcher struct {
	Store             *store.Store
	Embedder          embed.Provider
	SemanticThreshold float64
}

type candidate struct {
	SessionKey string
	Harness    string
	SessionID  string
	ParentID   string
	Name       string
	CWD        string
	StartedAt  int64
	UpdatedAt  int64
	SourcePath string
	EntryID    string
	Text       string
	RawScore   float64
}

type rankedChannel struct {
	name       string
	weight     float64
	candidates []candidate
}

func (s *Searcher) Search(ctx context.Context, query string, filters Filters) (Response, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Response{}, fmt.Errorf("search query is empty")
	}
	if filters.Limit <= 0 {
		filters.Limit = DefaultLimit
	}

	exact, err := s.exact(ctx, query, filters)
	if err != nil {
		return Response{}, err
	}
	lexical, err := s.lexical(ctx, query, filters)
	if err != nil {
		return Response{}, err
	}
	if !lexicalReliable(query, lexical) {
		lexical = nil
	}
	fuzzy, err := s.fuzzy(ctx, query, filters)
	if err != nil {
		return Response{}, err
	}

	var semantic []candidate
	var semanticErr error
	if s.Embedder != nil {
		semantic, semanticErr = s.semantic(ctx, query, filters)
	}
	response := Response{}
	if semanticErr != nil {
		response.SemanticError = semanticErr.Error()
	}
	response.Results = fuse(filters.Limit, []rankedChannel{
		{name: "exact", weight: 4, candidates: exact},
		{name: "lexical", weight: 1, candidates: lexical},
		{name: "fuzzy", weight: 1, candidates: fuzzy},
		{name: "semantic", weight: 1, candidates: semantic},
	})
	return response, nil
}

func (s *Searcher) exact(ctx context.Context, query string, filters Filters) ([]candidate, error) {
	where, args := filterSQL(filters, "s", "")
	statement := `
		SELECT s.key, s.harness, s.native_id, s.parent_id, s.name, s.cwd,
		       s.started_at, s.updated_at, s.source_path, '', ''
		FROM session AS s
		WHERE s.native_id = ?` + where + `
		ORDER BY s.updated_at DESC LIMIT ?`
	allArgs := []any{query}
	allArgs = append(allArgs, args...)
	allArgs = append(allArgs, candidateLimit)
	return queryCandidates(ctx, s.Store.DB(), statement, allArgs...)
}

func (s *Searcher) lexical(ctx context.Context, query string, filters Filters) ([]candidate, error) {
	match := lexicalQuery(query)
	if match == "" {
		return nil, nil
	}
	where, args := filterSQL(filters, "s", "c")
	statement := `
		SELECT s.key, s.harness, s.native_id, s.parent_id, s.name, s.cwd,
		       s.started_at, s.updated_at, s.source_path, c.entry_native_id, c.text,
		       bm25(chunk_fts, 1.0, 3.0, 0.5, 4.0, 1.0) AS score
		FROM chunk_fts
		JOIN chunk AS c ON c.id = chunk_fts.rowid
		JOIN session AS s ON s.key = c.session_key
		WHERE chunk_fts MATCH ?` + where + `
		ORDER BY score, s.updated_at DESC LIMIT ?`
	allArgs := []any{match}
	allArgs = append(allArgs, args...)
	allArgs = append(allArgs, candidateLimit)
	candidates, err := queryScoredCandidates(ctx, s.Store.DB(), statement, allArgs...)
	if err != nil {
		return nil, err
	}
	return filterLexicalCoverage(query, candidates), nil
}

func filterLexicalCoverage(query string, candidates []candidate) []candidate {
	queryTokens := uniqueTokens(query)
	minimum := 1
	if len(queryTokens) >= 3 {
		minimum = (len(queryTokens) + 2) / 3
	}
	filtered := candidates[:0]
	for _, item := range candidates {
		candidateTokens := uniqueTokens(item.Text + " " + item.Name + " " + item.CWD)
		matched := 0
		for token := range queryTokens {
			if _, ok := candidateTokens[token]; ok {
				matched++
			}
		}
		if matched >= minimum {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func uniqueTokens(text string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, token := range strings.Fields(chunk.Normalize(text)) {
		result[token] = struct{}{}
	}
	return result
}

func lexicalReliable(query string, candidates []candidate) bool {
	queryTokens := uniqueTokens(query)
	if len(queryTokens) < 3 {
		return true
	}
	maximum := 0
	for _, item := range candidates {
		candidateTokens := uniqueTokens(item.Text + " " + item.Name + " " + item.CWD)
		matched := 0
		for token := range queryTokens {
			if _, ok := candidateTokens[token]; ok {
				matched++
			}
		}
		if matched > maximum {
			maximum = matched
		}
	}
	return maximum*2 >= len(queryTokens)
}

func (s *Searcher) fuzzy(ctx context.Context, query string, filters Filters) ([]candidate, error) {
	querySet := chunk.GramSet(query)
	if len(querySet) == 0 {
		return nil, nil
	}
	grams := make([]string, 0, len(querySet))
	for gram := range querySet {
		grams = append(grams, quoteFTS(gram))
	}
	sort.Strings(grams)
	match := strings.Join(grams, " OR ")
	where, args := filterSQL(filters, "s", "c")
	statement := `
		SELECT s.key, s.harness, s.native_id, s.parent_id, s.name, s.cwd,
		       s.started_at, s.updated_at, s.source_path, c.entry_native_id, c.text, c.grams
		FROM chunk_grams
		JOIN chunk AS c ON c.id = chunk_grams.rowid
		JOIN session AS s ON s.key = c.session_key
		WHERE chunk_grams MATCH ?` + where + `
		ORDER BY bm25(chunk_grams), s.updated_at DESC LIMIT ?`
	allArgs := []any{match}
	allArgs = append(allArgs, args...)
	allArgs = append(allArgs, candidateLimit*2)
	rows, err := s.Store.DB().QueryContext(ctx, statement, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("fuzzy search: %w", err)
	}
	defer rows.Close()
	var candidates []candidate
	for rows.Next() {
		var item candidate
		var gramsText string
		if err := scanCandidate(rows, &item, &gramsText); err != nil {
			return nil, err
		}
		candidateSet := map[string]struct{}{}
		for _, gram := range strings.Fields(gramsText) {
			candidateSet[gram] = struct{}{}
		}
		item.RawScore = chunk.Similarity(querySet, candidateSet)
		if item.RawScore >= 0.25 {
			candidates = append(candidates, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].RawScore == candidates[j].RawScore {
			return candidates[i].UpdatedAt > candidates[j].UpdatedAt
		}
		return candidates[i].RawScore > candidates[j].RawScore
	})
	if len(candidates) > candidateLimit {
		candidates = candidates[:candidateLimit]
	}
	return candidates, nil
}

func (s *Searcher) semantic(ctx context.Context, query string, filters Filters) ([]candidate, error) {
	vectors, err := s.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	queryVector := vectors[0]
	where, args := filterSQL(filters, "s", "c")
	statement := `
		SELECT s.key, s.harness, s.native_id, s.parent_id, s.name, s.cwd,
		       s.started_at, s.updated_at, s.source_path, c.entry_native_id, c.text,
		       c.embedding
		FROM chunk AS c
		JOIN session AS s ON s.key = c.session_key
		WHERE c.embedding IS NOT NULL AND c.embedding_fingerprint = ?` + where
	allArgs := []any{s.Embedder.Fingerprint()}
	allArgs = append(allArgs, args...)
	rows, err := s.Store.DB().QueryContext(ctx, statement, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	defer rows.Close()

	top := &candidateHeap{}
	heap.Init(top)
	for rows.Next() {
		var item candidate
		var encoded []byte
		if err := scanCandidate(rows, &item, &encoded); err != nil {
			return nil, err
		}
		vector, err := embed.Decode(encoded)
		if err != nil || len(vector) != len(queryVector) {
			continue
		}
		item.RawScore, _ = embed.Dot(queryVector, vector)
		if item.RawScore < s.SemanticThreshold {
			continue
		}
		if top.Len() < candidateLimit {
			heap.Push(top, item)
		} else if item.RawScore > (*top)[0].RawScore {
			heap.Pop(top)
			heap.Push(top, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]candidate, top.Len())
	for index := len(result) - 1; index >= 0; index-- {
		result[index] = heap.Pop(top).(candidate)
	}
	return result, nil
}

func fuse(limit int, channels []rankedChannel) []Result {
	type aggregate struct {
		candidate candidate
		score     float64
		best      float64
		ranks     ChannelRanks
		semantic  float64
	}
	values := map[string]*aggregate{}
	for _, channel := range channels {
		seen := map[string]bool{}
		sessionRank := 0
		for _, item := range channel.candidates {
			if seen[item.SessionKey] {
				continue
			}
			seen[item.SessionKey] = true
			sessionRank++
			contribution := channel.weight / (rrfK + float64(sessionRank))
			value := values[item.SessionKey]
			if value == nil {
				value = &aggregate{candidate: item}
				values[item.SessionKey] = value
			}
			value.score += contribution
			if contribution > value.best || value.candidate.Text == "" {
				value.best = contribution
				if item.Text != "" || value.candidate.Text == "" {
					value.candidate = item
				}
			}
			switch channel.name {
			case "exact":
				value.ranks.Exact = sessionRank
			case "lexical":
				value.ranks.Lexical = sessionRank
			case "fuzzy":
				value.ranks.Fuzzy = sessionRank
			case "semantic":
				value.ranks.Semantic = sessionRank
				value.semantic = item.RawScore
			}
		}
	}
	type scored struct {
		value *aggregate
	}
	ordered := make([]scored, 0, len(values))
	for _, value := range values {
		ordered = append(ordered, scored{value: value})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].value.score == ordered[j].value.score {
			return ordered[i].value.candidate.UpdatedAt > ordered[j].value.candidate.UpdatedAt
		}
		return ordered[i].value.score > ordered[j].value.score
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	results := make([]Result, len(ordered))
	for index, item := range ordered {
		candidate := item.value.candidate
		results[index] = Result{
			Harness: candidate.Harness, SessionID: candidate.SessionID,
			ParentID: candidate.ParentID, Name: candidate.Name, CWD: candidate.CWD,
			StartedAt: fromMillis(candidate.StartedAt), UpdatedAt: fromMillis(candidate.UpdatedAt),
			SourcePath: candidate.SourcePath, EntryID: candidate.EntryID,
			Excerpt: excerpt(candidate.Text, 320), Resume: resumeHint(candidate),
			Ranks: item.value.ranks, SemanticSimilarity: item.value.semantic,
			CombinedRank: index + 1,
		}
	}
	return results
}

func filterSQL(filters Filters, sessionAlias, chunkAlias string) (string, []any) {
	var clauses []string
	var args []any
	if len(filters.Harnesses) > 0 {
		clauses = append(clauses, sessionAlias+`.harness IN (`+store.Placeholders(len(filters.Harnesses))+`)`)
		for _, value := range filters.Harnesses {
			args = append(args, value)
		}
	}
	if filters.Path != "" {
		path := strings.TrimRight(filters.Path, "/")
		clauses = append(clauses, `(`+sessionAlias+`.cwd = ? OR `+sessionAlias+`.cwd LIKE ? ESCAPE '\')`)
		args = append(args, path, escapeLike(path)+"/%")
	}
	if !filters.Since.IsZero() {
		clauses = append(clauses, sessionAlias+`.updated_at >= ?`)
		args = append(args, filters.Since.UnixMilli())
	}
	if !filters.Before.IsZero() {
		clauses = append(clauses, sessionAlias+`.updated_at <= ?`)
		args = append(args, filters.Before.UnixMilli())
	}
	if len(filters.Roles) > 0 && chunkAlias != "" {
		clauses = append(clauses, chunkAlias+`.role IN (`+store.Placeholders(len(filters.Roles))+`)`)
		for _, value := range filters.Roles {
			args = append(args, value)
		}
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

func lexicalQuery(query string) string {
	tokens := strings.Fields(chunk.Normalize(query))
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		parts = append(parts, quoteFTS(token))
	}
	return strings.Join(parts, " OR ")
}

func quoteFTS(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func queryCandidates(ctx context.Context, db *sql.DB, query string, args ...any) ([]candidate, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []candidate
	for rows.Next() {
		var item candidate
		if err := scanCandidate(rows, &item); err != nil {
			return nil, err
		}
		values = append(values, item)
	}
	return values, rows.Err()
}

func queryScoredCandidates(ctx context.Context, db *sql.DB, query string, args ...any) ([]candidate, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []candidate
	for rows.Next() {
		var item candidate
		if err := scanCandidate(rows, &item, &item.RawScore); err != nil {
			return nil, err
		}
		values = append(values, item)
	}
	return values, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanCandidate(rows scanner, item *candidate, extra ...any) error {
	base := []any{
		&item.SessionKey, &item.Harness, &item.SessionID, &item.ParentID,
		&item.Name, &item.CWD, &item.StartedAt, &item.UpdatedAt,
		&item.SourcePath, &item.EntryID, &item.Text,
	}
	base = append(base, extra...)
	return rows.Scan(base...)
}

func excerpt(text string, maximum int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= maximum {
		return text
	}
	return string(runes[:maximum-1]) + "…"
}

func resumeHint(item candidate) string {
	command := ""
	switch item.Harness {
	case "pi":
		command = "pi --session " + shellQuote(item.SessionID)
	case "codex":
		command = "codex resume " + shellQuote(item.SessionID)
	case "opencode":
		command = "opencode --session " + shellQuote(item.SessionID)
	case "claude":
		command = "claude --resume " + shellQuote(item.SessionID)
	}
	if item.CWD == "" {
		return command
	}
	return "cd " + shellQuote(item.CWD) + " && " + command
}

func shellQuote(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `'"'"'`) + `'`
}

func fromMillis(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

type candidateHeap []candidate

func (h candidateHeap) Len() int           { return len(h) }
func (h candidateHeap) Less(i, j int) bool { return h[i].RawScore < h[j].RawScore }
func (h candidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(value any)    { *h = append(*h, value.(candidate)) }
func (h *candidateHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}
