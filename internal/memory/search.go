package memory

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Thelost77/agent-sessions/internal/chunk"
	"github.com/Thelost77/agent-sessions/internal/embed"
)

const memoryCandidateLimit = 100
const memoryRRFK = 60.0

type Filters struct {
	ProjectKey      string
	AllProjects     bool
	GlobalOnly      bool
	IncludeArchived bool
	Limit           int
}

type ChannelRanks struct {
	Exact    int `json:"exact,omitempty"`
	Lexical  int `json:"lexical,omitempty"`
	Fuzzy    int `json:"fuzzy,omitempty"`
	Semantic int `json:"semantic,omitempty"`
}

type Result struct {
	ID                 string       `json:"id"`
	Scope              string       `json:"scope"`
	ProjectKey         string       `json:"projectKey,omitempty"`
	ProjectRemote      string       `json:"projectRemote,omitempty"`
	ProjectName        string       `json:"projectName,omitempty"`
	Source             string       `json:"source,omitempty"`
	CurrentRevision    int          `json:"currentRevision"`
	Content            string       `json:"content"`
	CreatedAt          time.Time    `json:"createdAt"`
	UpdatedAt          time.Time    `json:"updatedAt"`
	ArchivedAt         *time.Time   `json:"archivedAt,omitempty"`
	Ranks              ChannelRanks `json:"ranks,omitempty"`
	SemanticSimilarity float64      `json:"semanticSimilarity,omitempty"`
	CombinedRank       int          `json:"rank"`
}

type Response struct {
	Results       []Result `json:"results"`
	SemanticError string   `json:"semanticError,omitempty"`
}

type Searcher struct {
	Index             *Index
	Embedder          embed.Provider
	SemanticThreshold float64
}

type memoryCandidate struct {
	ID            string
	Scope         string
	ProjectKey    string
	ProjectRemote string
	ProjectName   string
	Source        string
	Revision      int
	Content       string
	CreatedAt     int64
	UpdatedAt     int64
	ArchivedAt    sql.NullInt64
	RawScore      float64
}

type memoryChannel struct {
	name       string
	weight     float64
	candidates []memoryCandidate
}

func (s *Searcher) Search(ctx context.Context, query string, filters Filters) (Response, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Response{}, fmt.Errorf("memory search query is empty")
	}
	if filters.Limit <= 0 {
		filters.Limit = 3
	}
	exact, err := s.exact(ctx, query, filters)
	if err != nil {
		return Response{}, err
	}
	lexical, err := s.lexical(ctx, query, filters)
	if err != nil {
		return Response{}, err
	}
	fuzzy, err := s.fuzzy(ctx, query, filters)
	if err != nil {
		return Response{}, err
	}
	var semantic []memoryCandidate
	var semanticErr error
	if s.Embedder != nil {
		semantic, semanticErr = s.semantic(ctx, query, filters)
	}
	response := Response{Results: []Result{}}
	if semanticErr != nil {
		response.SemanticError = semanticErr.Error()
	}
	response.Results = fuseMemory(filters.Limit, []memoryChannel{
		{name: "exact", weight: 4, candidates: exact},
		{name: "lexical", weight: 1, candidates: lexical},
		{name: "fuzzy", weight: 1, candidates: fuzzy},
		{name: "semantic", weight: 1, candidates: semantic},
	})
	return response, nil
}

func (s *Searcher) exact(ctx context.Context, query string, filters Filters) ([]memoryCandidate, error) {
	where, args := memoryFilterSQL(filters, "m")
	all := []any{query}
	all = append(all, args...)
	all = append(all, memoryCandidateLimit)
	return queryMemoryCandidates(ctx, s.Index.db, `
		SELECT m.id, m.scope, m.project_key, m.project_remote, m.project_name, m.source,
		       m.current_revision, m.content, m.created_at, m.updated_at, m.archived_at
		FROM memory AS m WHERE m.id = ?`+where+` ORDER BY m.updated_at DESC LIMIT ?`, all...)
}

func (s *Searcher) lexical(ctx context.Context, query string, filters Filters) ([]memoryCandidate, error) {
	match := memoryLexicalQuery(query)
	if match == "" {
		return nil, nil
	}
	where, args := memoryFilterSQL(filters, "m")
	all := []any{match}
	all = append(all, args...)
	all = append(all, memoryCandidateLimit)
	rows, err := s.Index.db.QueryContext(ctx, `
		SELECT m.id, m.scope, m.project_key, m.project_remote, m.project_name, m.source,
		       m.current_revision, m.content, m.created_at, m.updated_at, m.archived_at,
		       bm25(memory_fts, 1.0, 0.5, 0.5, 3.0)
		FROM memory_fts
		JOIN memory AS m ON m.rowid = memory_fts.rowid
		WHERE memory_fts MATCH ?`+where+`
		ORDER BY bm25(memory_fts), m.updated_at DESC LIMIT ?`, all...)
	if err != nil {
		return nil, fmt.Errorf("lexical memory search: %w", err)
	}
	defer rows.Close()
	var candidates []memoryCandidate
	queryTokens := tokenSet(query)
	minimum := 1
	if len(queryTokens) >= 3 {
		minimum = (len(queryTokens) + 2) / 3
	}
	for rows.Next() {
		var item memoryCandidate
		if err := scanMemoryCandidate(rows, &item, &item.RawScore); err != nil {
			return nil, err
		}
		candidateTokens := tokenSet(item.Content + " " + item.Source + " " + item.ProjectName)
		matched := 0
		for token := range queryTokens {
			if _, ok := candidateTokens[token]; ok {
				matched++
			}
		}
		if matched >= minimum {
			candidates = append(candidates, item)
		}
	}
	return candidates, rows.Err()
}

func (s *Searcher) fuzzy(ctx context.Context, query string, filters Filters) ([]memoryCandidate, error) {
	querySet := chunk.GramSet(query)
	if len(querySet) == 0 {
		return nil, nil
	}
	grams := make([]string, 0, len(querySet))
	for gram := range querySet {
		grams = append(grams, quoteMemoryFTS(gram))
	}
	sort.Strings(grams)
	where, args := memoryFilterSQL(filters, "m")
	all := []any{strings.Join(grams, " OR ")}
	all = append(all, args...)
	all = append(all, memoryCandidateLimit*2)
	rows, err := s.Index.db.QueryContext(ctx, `
		SELECT m.id, m.scope, m.project_key, m.project_remote, m.project_name, m.source,
		       m.current_revision, m.content, m.created_at, m.updated_at, m.archived_at, m.grams
		FROM memory_grams
		JOIN memory AS m ON m.rowid = memory_grams.rowid
		WHERE memory_grams MATCH ?`+where+`
		ORDER BY bm25(memory_grams), m.updated_at DESC LIMIT ?`, all...)
	if err != nil {
		return nil, fmt.Errorf("fuzzy memory search: %w", err)
	}
	defer rows.Close()
	var candidates []memoryCandidate
	for rows.Next() {
		var item memoryCandidate
		var gramsText string
		if err := scanMemoryCandidate(rows, &item, &gramsText); err != nil {
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
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].RawScore == candidates[right].RawScore {
			return candidates[left].UpdatedAt > candidates[right].UpdatedAt
		}
		return candidates[left].RawScore > candidates[right].RawScore
	})
	if len(candidates) > memoryCandidateLimit {
		candidates = candidates[:memoryCandidateLimit]
	}
	return candidates, nil
}

func (s *Searcher) semantic(ctx context.Context, query string, filters Filters) ([]memoryCandidate, error) {
	vectors, err := s.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedder returned no query vector")
	}
	where, args := memoryFilterSQL(filters, "m")
	all := []any{s.Embedder.Fingerprint()}
	all = append(all, args...)
	rows, err := s.Index.db.QueryContext(ctx, `
		SELECT m.id, m.scope, m.project_key, m.project_remote, m.project_name, m.source,
		       m.current_revision, m.content, m.created_at, m.updated_at, m.archived_at, m.embedding
		FROM memory AS m
		WHERE m.embedding IS NOT NULL AND m.embedding_fingerprint = ?`+where, all...)
	if err != nil {
		return nil, fmt.Errorf("semantic memory search: %w", err)
	}
	defer rows.Close()
	top := &memoryCandidateHeap{}
	heap.Init(top)
	for rows.Next() {
		var item memoryCandidate
		var encoded []byte
		if err := scanMemoryCandidate(rows, &item, &encoded); err != nil {
			return nil, err
		}
		vector, err := embed.Decode(encoded)
		if err != nil || len(vector) != len(vectors[0]) {
			continue
		}
		item.RawScore, _ = embed.Dot(vectors[0], vector)
		if item.RawScore < s.SemanticThreshold {
			continue
		}
		if top.Len() < memoryCandidateLimit {
			heap.Push(top, item)
		} else if item.RawScore > (*top)[0].RawScore {
			heap.Pop(top)
			heap.Push(top, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]memoryCandidate, top.Len())
	for position := len(result) - 1; position >= 0; position-- {
		result[position] = heap.Pop(top).(memoryCandidate)
	}
	return result, nil
}

func fuseMemory(limit int, channels []memoryChannel) []Result {
	type aggregate struct {
		candidate memoryCandidate
		score     float64
		best      float64
		ranks     ChannelRanks
		semantic  float64
	}
	values := map[string]*aggregate{}
	for _, channel := range channels {
		for rank, candidate := range channel.candidates {
			contribution := channel.weight / (memoryRRFK + float64(rank+1))
			value := values[candidate.ID]
			if value == nil {
				value = &aggregate{candidate: candidate}
				values[candidate.ID] = value
			}
			value.score += contribution
			if contribution > value.best {
				value.best = contribution
				value.candidate = candidate
			}
			switch channel.name {
			case "exact":
				value.ranks.Exact = rank + 1
			case "lexical":
				value.ranks.Lexical = rank + 1
			case "fuzzy":
				value.ranks.Fuzzy = rank + 1
			case "semantic":
				value.ranks.Semantic = rank + 1
				value.semantic = candidate.RawScore
			}
		}
	}
	ordered := make([]*aggregate, 0, len(values))
	for _, value := range values {
		ordered = append(ordered, value)
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].score == ordered[right].score {
			return ordered[left].candidate.UpdatedAt > ordered[right].candidate.UpdatedAt
		}
		return ordered[left].score > ordered[right].score
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	results := make([]Result, len(ordered))
	for position, value := range ordered {
		item := value.candidate
		result := Result{
			ID: item.ID, Scope: item.Scope, ProjectKey: item.ProjectKey,
			ProjectRemote: item.ProjectRemote, ProjectName: item.ProjectName,
			Source: item.Source, CurrentRevision: item.Revision, Content: item.Content,
			CreatedAt: time.UnixMilli(item.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(item.UpdatedAt).UTC(),
			Ranks: value.ranks, SemanticSimilarity: value.semantic, CombinedRank: position + 1,
		}
		if item.ArchivedAt.Valid {
			archived := time.UnixMilli(item.ArchivedAt.Int64).UTC()
			result.ArchivedAt = &archived
		}
		results[position] = result
	}
	return results
}

func memoryFilterSQL(filters Filters, alias string) (string, []any) {
	var clauses []string
	var args []any
	if !filters.IncludeArchived {
		clauses = append(clauses, alias+`.archived_at IS NULL`)
	}
	switch {
	case filters.AllProjects:
	case filters.GlobalOnly || filters.ProjectKey == "":
		clauses = append(clauses, alias+`.scope = 'global'`)
	default:
		clauses = append(clauses, `(`+alias+`.scope = 'global' OR (`+alias+`.scope = 'project' AND `+alias+`.project_key = ?))`)
		args = append(args, filters.ProjectKey)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

func queryMemoryCandidates(ctx context.Context, db *sql.DB, statement string, args ...any) ([]memoryCandidate, error) {
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []memoryCandidate
	for rows.Next() {
		var item memoryCandidate
		if err := scanMemoryCandidate(rows, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

type memoryScanner interface{ Scan(...any) error }

func scanMemoryCandidate(scanner memoryScanner, item *memoryCandidate, extra ...any) error {
	values := []any{
		&item.ID, &item.Scope, &item.ProjectKey, &item.ProjectRemote, &item.ProjectName,
		&item.Source, &item.Revision, &item.Content, &item.CreatedAt, &item.UpdatedAt, &item.ArchivedAt,
	}
	values = append(values, extra...)
	return scanner.Scan(values...)
}

func memoryLexicalQuery(query string) string {
	tokens := strings.Fields(chunk.Normalize(query))
	parts := make([]string, len(tokens))
	for position, token := range tokens {
		parts[position] = quoteMemoryFTS(token)
	}
	return strings.Join(parts, " OR ")
}

func quoteMemoryFTS(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func tokenSet(value string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, token := range strings.Fields(chunk.Normalize(value)) {
		result[token] = struct{}{}
	}
	return result
}

type memoryCandidateHeap []memoryCandidate

func (h memoryCandidateHeap) Len() int           { return len(h) }
func (h memoryCandidateHeap) Less(i, j int) bool { return h[i].RawScore < h[j].RawScore }
func (h memoryCandidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *memoryCandidateHeap) Push(value any)    { *h = append(*h, value.(memoryCandidate)) }
func (h *memoryCandidateHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}
