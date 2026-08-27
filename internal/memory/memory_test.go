package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixtureEmbedder struct {
	fingerprint string
	fail        bool
}

func (f fixtureEmbedder) Fingerprint() string { return f.fingerprint }
func (f fixtureEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.fail {
		return nil, errors.New("fixture embedding failure")
	}
	vectors := make([][]float32, len(texts))
	for index, text := range texts {
		normalized := strings.ToLower(text)
		switch {
		case strings.Contains(normalized, "database"), strings.Contains(normalized, "storage"):
			vectors[index] = []float32{1, 0}
		case strings.Contains(normalized, "weak"):
			vectors[index] = []float32{0.59, 0.807403}
		default:
			vectors[index] = []float32{0, 1}
		}
	}
	return vectors, nil
}

func newFixture(t *testing.T) (context.Context, FileStore, *Index) {
	t.Helper()
	root := t.TempDir()
	files := FileStore{Directory: filepath.Join(root, "canonical")}
	index, err := OpenIndex(context.Background(), filepath.Join(root, "local", "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return context.Background(), files, index
}

func createRecord(t *testing.T, files FileStore, scope, content, projectKey string) Record {
	t.Helper()
	name := ""
	remote := ""
	if scope == ScopeProject {
		name = "repository"
		remote = projectKey
	}
	record, err := files.Create(scope, content, "", projectKey, remote, name)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestLexicalFuzzyAndScopeSearchWorkOffline(t *testing.T) {
	ctx, files, index := newFixture(t)
	projectRecord := createRecord(t, files, ScopeProject, "We avoided SQLite triggers for durable database storage", "github.com/owner/repository")
	globalRecord := createRecord(t, files, ScopeGlobal, "Global keyboard shortcuts use the command key", "")
	otherRecord := createRecord(t, files, ScopeProject, "SQLite advice from another repository", "github.com/other/repository")
	if _, err := Refresh(ctx, files, index, nil); err != nil {
		t.Fatal(err)
	}
	searcher := &Searcher{Index: index}
	lexical, err := searcher.Search(ctx, "SQLite triggers", Filters{ProjectKey: "github.com/owner/repository", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(lexical.Results) != 1 || lexical.Results[0].ID != projectRecord.ID {
		t.Fatalf("project lexical results = %#v", lexical.Results)
	}
	exact, err := searcher.Search(ctx, projectRecord.ID, Filters{ProjectKey: "github.com/owner/repository", Limit: 10})
	if err != nil || len(exact.Results) != 1 || exact.Results[0].Ranks.Exact != 1 {
		t.Fatalf("exact ID results = %#v, %v", exact.Results, err)
	}
	fuzzy, err := searcher.Search(ctx, "keybord shortctus", Filters{ProjectKey: "github.com/owner/repository", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(fuzzy.Results) == 0 || fuzzy.Results[0].ID != globalRecord.ID {
		t.Fatalf("global fuzzy results = %#v", fuzzy.Results)
	}
	for _, result := range lexical.Results {
		if result.ID == otherRecord.ID {
			t.Fatal("another project's memory was included")
		}
	}
	all, err := searcher.Search(ctx, "SQLite advice", Filters{AllProjects: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Results) == 0 || all.Results[0].ID != otherRecord.ID {
		t.Fatalf("all-project results = %#v", all.Results)
	}
}

func TestSemanticSearchThresholdAndFingerprint(t *testing.T) {
	ctx, files, index := newFixture(t)
	strong := createRecord(t, files, ScopeGlobal, "database architecture decisions", "")
	_ = createRecord(t, files, ScopeGlobal, "weak semantic fixture", "")
	provider := fixtureEmbedder{fingerprint: "fixture:v1"}
	refresh, err := Refresh(ctx, files, index, provider)
	if err != nil {
		t.Fatal(err)
	}
	if refresh.Embedded != 2 {
		t.Fatalf("embedded = %d, want 2", refresh.Embedded)
	}
	response, err := (&Searcher{Index: index, Embedder: provider, SemanticThreshold: 0.60}).Search(ctx, "storage design", Filters{AllProjects: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].ID != strong.ID {
		t.Fatalf("semantic results = %#v", response.Results)
	}
	pending, err := index.Pending(ctx, "fixture:v2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending after fingerprint change = %d, want 2", len(pending))
	}
}

func TestEditArchiveRestoreAndEmbeddingInvalidation(t *testing.T) {
	ctx, files, index := newFixture(t)
	record := createRecord(t, files, ScopeGlobal, "database original content", "")
	provider := fixtureEmbedder{fingerprint: "fixture:v1"}
	if _, err := Refresh(ctx, files, index, provider); err != nil {
		t.Fatal(err)
	}
	edited, err := files.Edit(record.ID[:8], "replacement content")
	if err != nil {
		t.Fatal(err)
	}
	if edited.CurrentRevision != 2 || len(edited.Revisions) != 2 || edited.Revisions[0].Content != "database original content" {
		t.Fatalf("edited record = %#v", edited)
	}
	if _, err := Refresh(ctx, files, index, nil); err != nil {
		t.Fatal(err)
	}
	pending, err := index.Pending(ctx, provider.Fingerprint(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != record.ID {
		t.Fatalf("pending after edit = %#v, %v", pending, err)
	}
	if _, err := files.Forget(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(ctx, files, index, nil); err != nil {
		t.Fatal(err)
	}
	archivedSearch, err := (&Searcher{Index: index}).Search(ctx, "replacement", Filters{AllProjects: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(archivedSearch.Results) != 0 {
		t.Fatalf("archived result was returned: %#v", archivedSearch.Results)
	}
	included, err := (&Searcher{Index: index}).Search(ctx, "replacement", Filters{AllProjects: true, IncludeArchived: true, Limit: 10})
	if err != nil || len(included.Results) != 1 {
		t.Fatalf("included archived results = %#v, %v", included.Results, err)
	}
	if _, err := files.Restore(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(ctx, files, index, nil); err != nil {
		t.Fatal(err)
	}
	restored, err := (&Searcher{Index: index}).Search(ctx, "replacement", Filters{AllProjects: true, Limit: 10})
	if err != nil || len(restored.Results) != 1 {
		t.Fatalf("restored results = %#v, %v", restored.Results, err)
	}
}

func TestCanonicalSurvivesEmbeddingAndIndexFailuresAndRebuild(t *testing.T) {
	ctx, files, index := newFixture(t)
	record := createRecord(t, files, ScopeGlobal, "offline durable memory", "")
	refresh, err := Refresh(ctx, files, index, fixtureEmbedder{fingerprint: "fixture:v1", fail: true})
	if err != nil {
		t.Fatal(err)
	}
	if refresh.EmbeddingError == nil {
		t.Fatal("embedding failure was not reported")
	}
	if _, _, err := files.Load(record.ID); err != nil {
		t.Fatalf("canonical record was lost after embedding failure: %v", err)
	}
	pending, err := index.Pending(ctx, "fixture:v1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending memories = %#v, %v", pending, err)
	}
	indexPath := index.Path()
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(indexPath + suffix)
	}
	rebuilt, err := OpenIndex(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if _, err := Refresh(ctx, files, rebuilt, nil); err != nil {
		t.Fatal(err)
	}
	response, err := (&Searcher{Index: rebuilt}).Search(ctx, "durable memory", Filters{AllProjects: true, Limit: 10})
	if err != nil || len(response.Results) != 1 || response.Results[0].ID != record.ID {
		t.Fatalf("rebuilt results = %#v, %v", response.Results, err)
	}
	badIndex := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badIndex, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenIndex(ctx, filepath.Join(badIndex, "index.sqlite")); err == nil {
		t.Fatal("invalid index path unexpectedly opened")
	}
	if _, _, err := files.Load(record.ID); err != nil {
		t.Fatalf("canonical record was lost after index failure: %v", err)
	}
}

func TestInvalidChangedRecordRetainsLastGoodIndex(t *testing.T) {
	ctx, files, index := newFixture(t)
	record := createRecord(t, files, ScopeGlobal, "last known valid content", "")
	if _, err := Refresh(ctx, files, index, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(files.RecordsDirectory(), record.ID+".json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"id":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	refresh, err := Refresh(ctx, files, index, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(refresh.Warnings) != 1 || refresh.Warnings[0].Type != "invalid_record" {
		t.Fatalf("warnings = %#v", refresh.Warnings)
	}
	response, err := (&Searcher{Index: index}).Search(ctx, "last known valid", Filters{AllProjects: true, Limit: 10})
	if err != nil || len(response.Results) != 1 || response.Results[0].ID != record.ID {
		t.Fatalf("last good result = %#v, %v", response.Results, err)
	}
}

func TestSyncthingConflictIsReportedIgnoredAndBlocksMutation(t *testing.T) {
	ctx, files, index := newFixture(t)
	record := createRecord(t, files, ScopeGlobal, "canonical conflict content", "")
	canonical := filepath.Join(files.RecordsDirectory(), record.ID+".json")
	conflict := filepath.Join(files.RecordsDirectory(), record.ID+".sync-conflict-20260821-120000-ABC1234.json")
	conflictPayload := []byte("do not modify this conflict")
	if err := os.WriteFile(conflict, conflictPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(conflict)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := Refresh(ctx, files, index, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(refresh.Warnings) != 1 || refresh.Warnings[0].Type != "syncthing_conflict" || refresh.Warnings[0].Path != canonical || len(refresh.Warnings[0].ConflictPaths) != 1 {
		t.Fatalf("conflict warning = %#v", refresh.Warnings)
	}
	var indexed int
	if err := index.db.QueryRowContext(ctx, `SELECT count(*) FROM memory`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 1 {
		t.Fatalf("indexed rows = %d, want only canonical row", indexed)
	}
	for _, mutate := range []func(string) error{
		func(id string) error { _, err := files.Edit(id, "changed"); return err },
		func(id string) error { _, err := files.Forget(id); return err },
		func(id string) error { _, err := files.Restore(id); return err },
	} {
		if err := mutate(record.ID); err == nil || !strings.Contains(err.Error(), "Syncthing conflicts") {
			t.Fatalf("conflicted mutation error = %v", err)
		}
	}
	afterPayload, err := os.ReadFile(conflict)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(conflict)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterPayload) != string(conflictPayload) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("conflict file was modified")
	}
}

func TestPrefixResolutionIsUnambiguous(t *testing.T) {
	root := t.TempDir()
	files := FileStore{Directory: root, Now: func() time.Time { return time.Unix(100, 0).UTC() }}
	for _, id := range []string{"m_abcdef111111", "m_abcdef222222"} {
		record := Record{
			SchemaVersion: SchemaVersion, ID: id, Scope: ScopeGlobal,
			CreatedAt: files.now(), UpdatedAt: files.now(), CurrentRevision: 1,
			Revisions: []Revision{{Revision: 1, Content: id, CreatedAt: files.now()}},
		}
		if err := files.write(record); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := files.Load("m_abcdef"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix error = %v", err)
	}
	record, _, err := files.Load("m_abcdef1")
	if err != nil || record.ID != "m_abcdef111111" {
		t.Fatalf("unique prefix result = %#v, %v", record, err)
	}
}

func TestCreatedDataHasRestrictivePermissionsAndCanonicalHasNoIndexData(t *testing.T) {
	ctx, files, index := newFixture(t)
	if err := EnsureIndexOutsideCanonical(files.Directory, filepath.Join(files.Directory, "index.sqlite")); err == nil {
		t.Fatal("index path inside canonical directory was accepted")
	}
	record := createRecord(t, files, ScopeGlobal, "permission fixture", "")
	if _, err := Refresh(ctx, files, index, fixtureEmbedder{fingerprint: "fixture:v1"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{files.Directory, files.RecordsDirectory()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", path, info.Mode().Perm())
		}
	}
	info, err := os.Stat(filepath.Join(files.RecordsDirectory(), record.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %o, want 600", info.Mode().Perm())
	}
	indexInfo, err := os.Stat(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	if indexInfo.Mode().Perm() != 0o600 {
		t.Fatalf("memory index mode = %o, want 600", indexInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(files.Directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if strings.Contains(name, "sqlite") || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.Contains(name, "embedding") {
			t.Fatalf("derived data entered canonical directory: %s", entry.Name())
		}
	}
}
