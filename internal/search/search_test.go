package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Thelost77/recall/internal/chunk"
	"github.com/Thelost77/recall/internal/embed"
	"github.com/Thelost77/recall/internal/model"
	"github.com/Thelost77/recall/internal/store"
)

type semanticFixture struct{}

func (semanticFixture) Fingerprint() string { return "fixture:v1" }
func (semanticFixture) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}

func TestHybridSearchFindsSessionAndDeduplicatesChunks(t *testing.T) {
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	source := model.Source{Key: "source", Harness: "pi", Path: "/sessions/pi.jsonl", Version: "1"}
	sessions := []model.Session{
		{Key: "qr", Harness: "pi", NativeID: "qr-session", Name: "QR generation", CWD: "/work/QrCodes", SourceKey: "source", SourcePath: source.Path, UpdatedAt: time.Unix(20, 0)},
		{Key: "invoice", Harness: "pi", NativeID: "invoice-session", Name: "Invoices", CWD: "/work/billing", SourceKey: "source", SourcePath: source.Path, UpdatedAt: time.Unix(10, 0)},
	}
	entries := []model.Entry{
		{Key: "qr-1", SessionKey: "qr", NativeID: "entry-qr-1", Role: "user", Kind: "message", Text: "Generate QR codes without assets and print them later"},
		{Key: "qr-2", SessionKey: "qr", NativeID: "entry-qr-2", Role: "assistant", Kind: "message", Text: "Open the empty-state generation dialog"},
		{Key: "invoice-1", SessionKey: "invoice", NativeID: "entry-invoice", Role: "user", Kind: "message", Text: "Investigate an invoice payment failure"},
	}
	if err := storage.UpsertSource(ctx, source, model.ParsedSource{Sessions: sessions, Entries: entries}, chunk.Entries(entries)); err != nil {
		t.Fatal(err)
	}

	pending, err := storage.PendingChunks(ctx, "fixture:v1", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(pending))
	vectors := make([][]byte, len(pending))
	for index, item := range pending {
		ids[index] = item.ID
		vector := []float32{0, 1}
		if strings.Contains(item.Text, "QR") || strings.Contains(item.Text, "empty-state") {
			vector = []float32{1, 0}
		}
		vectors[index] = embed.Encode(vector)
	}
	if err := storage.UpdateEmbeddings(ctx, "fixture:v1", 2, ids, vectors); err != nil {
		t.Fatal(err)
	}

	response, err := (&Searcher{Store: storage, Embedder: semanticFixture{}}).Search(ctx, "print stickers before assignment", Filters{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 || response.Results[0].SessionID != "qr-session" {
		t.Fatalf("unexpected semantic results: %#v", response.Results)
	}
	if response.Results[0].SemanticSimilarity != 1 {
		t.Fatalf("semantic similarity = %f, want 1", response.Results[0].SemanticSimilarity)
	}
	seen := 0
	for _, result := range response.Results {
		if result.SessionID == "qr-session" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("QR session appeared %d times", seen)
	}
}

func TestSemanticThresholdExcludesWeakCandidates(t *testing.T) {
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	source := model.Source{Key: "source", Harness: "pi", Path: "/sessions/pi.jsonl", Version: "1"}
	sessions := []model.Session{
		{Key: "strong", Harness: "pi", NativeID: "strong-session", Name: "Strong", SourceKey: source.Key, SourcePath: source.Path},
		{Key: "weak", Harness: "pi", NativeID: "weak-session", Name: "Weak", SourceKey: source.Key, SourcePath: source.Path},
	}
	entries := []model.Entry{
		{Key: "strong-entry", SessionKey: "strong", NativeID: "strong-entry", Role: "user", Kind: "message", Text: "alpha"},
		{Key: "weak-entry", SessionKey: "weak", NativeID: "weak-entry", Role: "user", Kind: "message", Text: "beta"},
	}
	if err := storage.UpsertSource(ctx, source, model.ParsedSource{Sessions: sessions, Entries: entries}, chunk.Entries(entries)); err != nil {
		t.Fatal(err)
	}

	pending, err := storage.PendingChunks(ctx, "fixture:v1", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(pending))
	vectors := make([][]byte, len(pending))
	for index, item := range pending {
		ids[index] = item.ID
		vector := []float32{0.59, 0.807403}
		if item.Text == "alpha" {
			vector = []float32{0.61, 0.792401}
		}
		vectors[index] = embed.Encode(vector)
	}
	if err := storage.UpdateEmbeddings(ctx, "fixture:v1", 2, ids, vectors); err != nil {
		t.Fatal(err)
	}

	searcher := &Searcher{Store: storage, Embedder: semanticFixture{}, SemanticThreshold: 0.60}
	candidates, err := searcher.semantic(ctx, "unrelated", Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].SessionID != "strong-session" {
		t.Fatalf("semantic candidates: %#v", candidates)
	}
}

func TestSearchDefaultsToThreeResults(t *testing.T) {
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	source := model.Source{Key: "source", Harness: "pi", Path: "/sessions/pi.jsonl", Version: "1"}
	var sessions []model.Session
	var entries []model.Entry
	for index := 0; index < 4; index++ {
		key := string(rune('a' + index))
		sessions = append(sessions, model.Session{Key: key, Harness: "pi", NativeID: key, Name: "Shared result", SourceKey: source.Key, SourcePath: source.Path})
		entries = append(entries, model.Entry{Key: key + "-entry", SessionKey: key, NativeID: key + "-entry", Role: "user", Kind: "message", Text: "shared search phrase"})
	}
	if err := storage.UpsertSource(ctx, source, model.ParsedSource{Sessions: sessions, Entries: entries}, chunk.Entries(entries)); err != nil {
		t.Fatal(err)
	}

	response, err := (&Searcher{Store: storage}).Search(ctx, "shared search phrase", Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 3 {
		t.Fatalf("default result count = %d, want 3", len(response.Results))
	}
}

func TestGrokResumeHintUsesSessionID(t *testing.T) {
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source := model.Source{Key: "source", Harness: "grok", Path: "/sessions/updates.jsonl", Version: "1"}
	session := model.Session{
		Key: "session", Harness: "grok", NativeID: "01a0-session", Name: "TPM",
		CWD: "/work/kumu", SourceKey: source.Key, SourcePath: source.Path,
	}
	entry := model.Entry{Key: "entry", SessionKey: session.Key, NativeID: "message", Role: "user", Kind: "message", Text: "set up tpm"}
	if err := storage.UpsertSource(ctx, source, model.ParsedSource{Sessions: []model.Session{session}, Entries: []model.Entry{entry}}, chunk.Entries([]model.Entry{entry})); err != nil {
		t.Fatal(err)
	}

	response, err := (&Searcher{Store: storage}).Search(ctx, "01a0-session", Filters{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 {
		t.Fatal("expected a resume result")
	}
	want := "cd '/work/kumu' && grok --resume '01a0-session'"
	if response.Results[0].Resume != want {
		t.Fatalf("resume = %q, want %q", response.Results[0].Resume, want)
	}
}

func TestFuzzyExactAndPathFilters(t *testing.T) {
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source := model.Source{Key: "source", Harness: "codex", Path: "/session", Version: "1"}
	session := model.Session{Key: "session", Harness: "codex", NativeID: "abc-123", Name: "Empty state", CWD: "/work/QrCodes/ui", SourceKey: source.Key, SourcePath: source.Path}
	entry := model.Entry{Key: "entry", SessionKey: session.Key, NativeID: "message", Role: "user", Kind: "message", Text: "Generate QR codes from the empty state"}
	if err := storage.UpsertSource(ctx, source, model.ParsedSource{Sessions: []model.Session{session}, Entries: []model.Entry{entry}}, chunk.Entries([]model.Entry{entry})); err != nil {
		t.Fatal(err)
	}

	searcher := &Searcher{Store: storage}
	response, err := searcher.Search(ctx, "genrate qr cdoes emtpy state", Filters{Path: "/work/QrCodes", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 || response.Results[0].SessionID != "abc-123" {
		t.Fatalf("fuzzy results: %#v", response.Results)
	}

	exact, err := searcher.Search(ctx, "abc-123", Filters{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Results) == 0 || exact.Results[0].Ranks.Exact != 1 {
		t.Fatalf("exact results: %#v", exact.Results)
	}

	filtered, err := searcher.Search(ctx, "empty state", Filters{Path: "/other", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Results) != 0 {
		t.Fatalf("path filter returned %#v", filtered.Results)
	}
}
