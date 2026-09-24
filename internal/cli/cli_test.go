package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Thelost77/recall/internal/chunk"
	"github.com/Thelost77/recall/internal/config"
	"github.com/Thelost77/recall/internal/memory"
	"github.com/Thelost77/recall/internal/model"
	"github.com/Thelost77/recall/internal/store"
)

func fixtureConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		Index: filepath.Join(root, "sessions.sqlite"), MemoryDirectory: filepath.Join(root, "memories"),
		MemoryIndex: filepath.Join(root, "memory.sqlite"),
		Embedding:   config.EmbeddingConfig{URL: "http://127.0.0.1:1", Model: "fixture", Threshold: 0.60},
		Harnesses:   map[string]bool{"pi": true},
	}
}

func populateCLIIndexes(t *testing.T, cfg config.Config) {
	t.Helper()
	ctx := context.Background()
	sessions, err := store.Open(ctx, cfg.Index)
	if err != nil {
		t.Fatal(err)
	}
	source := model.Source{Key: "source", Harness: "pi", Path: "/fixture/session.jsonl", Version: "1"}
	session := model.Session{Key: "session", Harness: "pi", NativeID: "session-1", Name: "Fixture session", CWD: "/work/repository", SourceKey: source.Key, SourcePath: source.Path}
	entry := model.Entry{Key: "entry", SessionKey: session.Key, NativeID: "entry-1", Role: "user", Kind: "message", Text: "shared routing needle from session"}
	if err := sessions.UpsertSource(ctx, source, model.ParsedSource{Sessions: []model.Session{session}, Entries: []model.Entry{entry}}, chunk.Entries([]model.Entry{entry})); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	files := memory.FileStore{Directory: cfg.MemoryDirectory}
	if _, err := files.Create(memory.ScopeGlobal, "shared routing needle from memory", "fixture", "", "", ""); err != nil {
		t.Fatal(err)
	}
	memoryIndex, err := memory.OpenIndex(ctx, cfg.MemoryIndex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Refresh(ctx, files, memoryIndex, nil); err != nil {
		t.Fatal(err)
	}
	if err := memoryIndex.Close(); err != nil {
		t.Fatal(err)
	}
}

func runRecallFixture(t *testing.T, cfg config.Config, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Getwd: func() (string, error) { return t.TempDir(), nil }, Version: "dev"}
	err := app.runRecall(context.Background(), cfg, "fixture.toml", args)
	return stdout.String(), stderr.String(), err
}

func TestRecallShortAndLongSessionRoutesAreEquivalent(t *testing.T) {
	cfg := fixtureConfig(t)
	populateCLIIndexes(t, cfg)
	short, _, err := runRecallFixture(t, cfg, "-s", "--lexical-only", "--all-projects", "--json", "routing needle")
	if err != nil {
		t.Fatal(err)
	}
	long, _, err := runRecallFixture(t, cfg, "session", "--lexical-only", "--all-projects", "--json", "routing needle")
	if err != nil {
		t.Fatal(err)
	}
	if short != long {
		t.Fatalf("short and long session output differ\nshort: %s\nlong: %s", short, long)
	}
}

func TestRecallShortAndLongMemoryRoutesAreEquivalent(t *testing.T) {
	cfg := fixtureConfig(t)
	populateCLIIndexes(t, cfg)
	short, _, err := runRecallFixture(t, cfg, "-m", "--lexical-only", "--all-projects", "--json", "routing needle")
	if err != nil {
		t.Fatal(err)
	}
	long, _, err := runRecallFixture(t, cfg, "memory", "--lexical-only", "--all-projects", "--json", "routing needle")
	if err != nil {
		t.Fatal(err)
	}
	if short != long {
		t.Fatalf("short and long memory output differ\nshort: %s\nlong: %s", short, long)
	}
}

func TestBareRecallReturnsSeparateGroups(t *testing.T) {
	cfg := fixtureConfig(t)
	populateCLIIndexes(t, cfg)
	output, _, err := runRecallFixture(t, cfg, "--lexical-only", "--all-projects", "--json", "routing needle")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Query    string          `json:"query"`
		Memories memory.Response `json:"memories"`
		Sessions struct {
			Results []json.RawMessage `json:"results"`
		} `json:"sessions"`
		Warnings []memory.Warning `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Query != "routing needle" || len(payload.Memories.Results) != 1 || len(payload.Sessions.Results) != 1 {
		t.Fatalf("combined payload = %#v", payload)
	}
	if payload.Warnings == nil {
		t.Fatal("warnings must be a JSON array")
	}
}

func TestSessionSearchTextAndJSONShape(t *testing.T) {
	cfg := fixtureConfig(t)
	populateCLIIndexes(t, cfg)
	var text bytes.Buffer
	app := &App{Stdin: strings.NewReader(""), Stdout: &text, Stderr: &bytes.Buffer{}, Getwd: os.Getwd, Version: "dev"}
	if err := app.RunSessionSearch(context.Background(), cfg, []string{"--lexical-only", "routing needle"}, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.String(), "SESSIONS") || !strings.Contains(text.String(), "1. [pi] Fixture session") || !strings.Contains(text.String(), "Resume:") {
		t.Fatalf("session text output changed: %s", text.String())
	}
	var jsonOutput bytes.Buffer
	app.Stdout = &jsonOutput
	if err := app.RunSessionSearch(context.Background(), cfg, []string{"--lexical-only", "--json", "routing needle"}, ""); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(jsonOutput.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["results"]; !ok {
		t.Fatalf("session JSON has no results: %s", jsonOutput.String())
	}
	for _, unexpected := range []string{"query", "memories", "sessions", "warnings"} {
		if _, ok := payload[unexpected]; ok {
			t.Fatalf("session JSON gained %q: %s", unexpected, jsonOutput.String())
		}
	}
}

func TestRememberSucceedsWhenEmbeddingFails(t *testing.T) {
	cfg := fixtureConfig(t)
	var stdout, stderr bytes.Buffer
	app := &App{Stdin: strings.NewReader("piped durable content"), Stdout: &stdout, Stderr: &stderr, Getwd: os.Getwd, Version: "dev"}
	if err := app.runRecall(context.Background(), cfg, "fixture.toml", []string{"remember", "--global"}); err != nil {
		t.Fatal(err)
	}
	scan, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Records) != 1 || scan.Records[0].Record.CurrentContent() != "piped durable content" {
		t.Fatalf("canonical records = %#v", scan.Records)
	}
	index, err := memory.OpenIndex(context.Background(), cfg.MemoryIndex)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	pending, err := index.Pending(context.Background(), "ollama:fixture:chunks-v1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v, %v", pending, err)
	}
	if !strings.Contains(stderr.String(), "embedding remains pending") {
		t.Fatalf("missing degraded warning: %s", stderr.String())
	}
}

func TestRecallRejectsInapplicableAndConflictingOptions(t *testing.T) {
	cfg := fixtureConfig(t)
	if _, _, err := runRecallFixture(t, cfg, "-s", "-m", "query"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("-s/-m error = %v", err)
	}
	if _, _, err := runRecallFixture(t, cfg, "session", "--include-archived", "query"); err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("session option error = %v", err)
	}
	if _, _, err := runRecallFixture(t, cfg, "memory", "--path", "/tmp", "query"); err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("memory option error = %v", err)
	}
}

func TestCombinedJSONContainsStructuredConflictWarning(t *testing.T) {
	cfg := fixtureConfig(t)
	populateCLIIndexes(t, cfg)
	scan, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Scan()
	if err != nil || len(scan.Records) != 1 {
		t.Fatal(err)
	}
	id := scan.Records[0].Record.ID
	conflict := filepath.Join(cfg.MemoryDirectory, "records", id+".sync-conflict-20260821-120000-ABC1234.json")
	if err := os.WriteFile(conflict, []byte("conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, _, err := runRecallFixture(t, cfg, "--lexical-only", "--all-projects", "--json", "routing needle")
	if err != nil {
		t.Fatal(err)
	}
	var payload recallSearchOutput
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Warnings) != 1 || payload.Warnings[0].Type != "syncthing_conflict" || payload.Warnings[0].MemoryID != id || len(payload.Warnings[0].ConflictPaths) != 1 {
		t.Fatalf("structured warnings = %#v", payload.Warnings)
	}
}

func TestSessionRebuildDoesNotModifyCanonicalMemories(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Sources.Pi = filepath.Join(t.TempDir(), "pi-sessions")
	if err := os.MkdirAll(cfg.Sources.Pi, 0o700); err != nil {
		t.Fatal(err)
	}
	files := memory.FileStore{Directory: cfg.MemoryDirectory}
	record, err := files.Create(memory.ScopeGlobal, "canonical rebuild fixture", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(files.RecordsDirectory(), record.ID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Getwd: os.Getwd, Version: "dev"}
	if err := app.runSessionIndex(context.Background(), cfg, []string{"--rebuild", "--quiet"}, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("session rebuild modified canonical memory")
	}
}

func TestRecallHasNoConflictResolutionCommand(t *testing.T) {
	cfg := fixtureConfig(t)
	output, _, err := runRecallFixture(t, cfg, "help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "resolve") {
		t.Fatalf("help advertises a conflict resolution command: %s", output)
	}
}

func TestRecallShortVersionFlagPrintsVersion(t *testing.T) {
	cfg := fixtureConfig(t)
	want, _, err := runRecallFixture(t, cfg, "version")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := runRecallFixture(t, cfg, "-v")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("-v output = %q, want %q", got, want)
	}
}
