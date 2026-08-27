package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Thelost77/recall/internal/model"
	"github.com/Thelost77/recall/internal/store"
)

type fakeAdapter struct {
	version string
	entries []model.Entry
}

func (f *fakeAdapter) Name() string { return "pi" }
func (f *fakeAdapter) Discover(context.Context) ([]model.Source, error) {
	return []model.Source{{Key: "source", Harness: "pi", Path: "/fixture", Version: f.version}}, nil
}
func (f *fakeAdapter) Parse(context.Context, model.Source) (model.ParsedSource, error) {
	session := model.Session{
		Key: "session", Harness: "pi", NativeID: "native", Name: "Fixture",
		CWD: "/work", SourceKey: "source", SourcePath: "/fixture",
		StartedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0),
	}
	entries := append([]model.Entry(nil), f.entries...)
	for index := range entries {
		entries[index].SessionKey = session.Key
	}
	return model.ParsedSource{Sessions: []model.Session{session}, Entries: entries}, nil
}

type fakeEmbedder struct{ calls, texts int }

func (f *fakeEmbedder) Fingerprint() string { return "fake:v1" }
func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.texts += len(texts)
	vectors := make([][]float32, len(texts))
	for index := range vectors {
		vectors[index] = []float32{1, 0}
	}
	return vectors, nil
}

func TestIndexLockRejectsConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.lock")
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := AcquireLock(path); err == nil {
		t.Fatal("second writer acquired the index lock")
	}
}

func TestIncrementalIndexPreservesEmbeddingsForUnchangedText(t *testing.T) {
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	adapter := &fakeAdapter{version: "1", entries: []model.Entry{{Key: "entry-1", NativeID: "one", Role: "user", Kind: "message", Text: "Generate QR codes"}}}
	embedder := &fakeEmbedder{}
	value := &Indexer{Store: storage, Adapters: []model.Adapter{adapter}, Embedder: embedder}
	first, err := value.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if first.UpdatedSources != 1 || embedder.texts != 1 {
		t.Fatalf("first run = %#v, embedded=%d", first, embedder.texts)
	}

	second, err := value.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if second.UnchangedSources != 1 || embedder.texts != 1 {
		t.Fatalf("second run = %#v, embedded=%d", second, embedder.texts)
	}

	adapter.version = "2"
	third, err := value.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if third.UpdatedSources != 1 || third.EmbeddedChunks != 0 || embedder.texts != 1 {
		t.Fatalf("metadata-only run = %#v, embedded=%d", third, embedder.texts)
	}

	adapter.version = "3"
	adapter.entries = append(adapter.entries, model.Entry{Key: "entry-2", NativeID: "two", Role: "assistant", Kind: "message", Text: "Use an empty-state dialog"})
	fourth, err := value.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if fourth.EmbeddedChunks != 1 || embedder.texts != 2 {
		t.Fatalf("append run = %#v, embedded=%d", fourth, embedder.texts)
	}
}
