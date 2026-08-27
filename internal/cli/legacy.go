package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/adapter"
	"github.com/Thelost77/recall/internal/config"
	"github.com/Thelost77/recall/internal/embed"
	"github.com/Thelost77/recall/internal/indexer"
	"github.com/Thelost77/recall/internal/model"
	"github.com/Thelost77/recall/internal/search"
	"github.com/Thelost77/recall/internal/store"
)

func (a *App) runAgentSessions(ctx context.Context, cfg config.Config, configPath string, args []string) error {
	if len(args) == 0 {
		a.printAgentUsage()
		return nil
	}
	switch args[0] {
	case "index":
		return a.runSessionIndex(ctx, cfg, args[1:], false)
	case "search":
		return a.RunSessionSearch(ctx, cfg, args[1:], true, "")
	case "status":
		return a.runSessionStatus(ctx, cfg, args[1:], false)
	case "doctor":
		return a.runSessionDoctor(ctx, cfg, configPath, args[1:], false)
	case "version", "--version", "-version":
		fmt.Fprintln(a.Stdout, BuildVersion(a.Version))
		return nil
	case "help", "--help", "-h":
		a.printAgentUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a *App) runSessionIndex(ctx context.Context, cfg config.Config, args []string, recall bool) error {
	flags := flag.NewFlagSet("index", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	rebuild := flags.Bool("rebuild", false, "rebuild the complete index")
	reembed := flags.Bool("reembed", false, "regenerate every embedding")
	quiet := flags.Bool("quiet", false, "suppress progress messages")
	harnesses := flags.String("harness", "", "comma-separated harness names")
	addSessionCommonFlags(flags, &cfg)
	if recall {
		addMemoryCommonFlags(flags, &cfg)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("index does not accept positional arguments")
	}
	selected, err := selectHarnesses(cfg, *harnesses)
	if err != nil {
		return err
	}
	lock, err := indexer.AcquireLock(cfg.Index + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if *rebuild {
		if err := removeSQLite(cfg.Index); err != nil {
			return fmt.Errorf("remove old index: %w", err)
		}
	}
	storage, err := store.Open(ctx, cfg.Index)
	if err != nil {
		return err
	}
	defer storage.Close()
	embedder, err := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
	if err != nil {
		return err
	}
	adapters, closeAdapters, err := makeAdapters(ctx, cfg, selected)
	if err != nil {
		return err
	}
	defer closeAdapters()
	progress := func(string) {}
	if !*quiet {
		progress = func(message string) { fmt.Fprintln(a.Stderr, message) }
	}
	result, runErr := (&indexer.Indexer{Store: storage, Adapters: adapters, Embedder: embedder}).Run(ctx, indexer.Options{Reembed: *reembed, LockHeld: true, Progress: progress})
	if !*quiet {
		fmt.Fprintf(a.Stdout, "Discovered: %d\nUpdated: %d\nUnchanged: %d\nDeleted: %d\nParser errors: %d\nEmbedded: %d\n",
			result.Discovered, result.UpdatedSources, result.UnchangedSources,
			result.DeletedSources, result.ParserErrors, result.EmbeddedChunks)
	}
	if result.EmbeddingError != nil {
		fmt.Fprintln(a.Stderr, "warning: semantic indexing is incomplete:", result.EmbeddingError)
	}
	return runErr
}

func (a *App) RunSessionSearch(ctx context.Context, cfg config.Config, args []string, legacy bool, defaultPath string) error {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	path := flags.String("path", defaultPath, "exact directory or parent directory")
	harnesses := flags.String("harness", "", "comma-separated harness names")
	roles := flags.String("role", "", "comma-separated roles")
	since := flags.String("since", "", "minimum session date")
	before := flags.String("before", "", "maximum session date")
	limit := flags.Int("limit", search.DefaultLimit, "maximum session results")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	explain := flags.Bool("explain", false, "show retrieval channel ranks")
	lexicalOnly := flags.Bool("lexical-only", false, "skip semantic retrieval")
	semanticThreshold := flags.Float64("semantic-threshold", cfg.Embedding.Threshold, "minimum semantic cosine similarity")
	addSessionCommonFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	query := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if query == "" {
		return fmt.Errorf("search requires a query")
	}
	if err := validateSearchValues(*limit, *semanticThreshold); err != nil {
		return err
	}
	selected, err := parseHarnessList(*harnesses)
	if err != nil {
		return err
	}
	roleValues, err := parseRoles(*roles)
	if err != nil {
		return err
	}
	sinceTime, err := parseDate(*since, false)
	if err != nil {
		return fmt.Errorf("invalid --since: %w", err)
	}
	beforeTime, err := parseDate(*before, true)
	if err != nil {
		return fmt.Errorf("invalid --before: %w", err)
	}
	if *path != "" {
		absolute, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		*path = filepath.Clean(absolute)
	}
	response, err := searchSessions(ctx, cfg, query, sessionSearchOptions{
		Path: *path, Harnesses: selected, Roles: roleValues, Since: sinceTime,
		Before: beforeTime, Limit: *limit, LexicalOnly: *lexicalOnly,
		SemanticThreshold: *semanticThreshold,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(a.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(response)
	}
	if response.SemanticError != "" {
		fmt.Fprintln(a.Stderr, "warning: semantic search unavailable:", response.SemanticError)
	}
	a.printSessionResults(response.Results, *explain)
	_ = legacy
	return nil
}

type sessionSearchOptions struct {
	Path              string
	Harnesses         []string
	Roles             []string
	Since             time.Time
	Before            time.Time
	Limit             int
	LexicalOnly       bool
	SemanticThreshold float64
}

func searchSessions(ctx context.Context, cfg config.Config, query string, options sessionSearchOptions) (search.Response, error) {
	storage, err := store.Open(ctx, cfg.Index)
	if err != nil {
		return search.Response{}, err
	}
	defer storage.Close()
	var provider embed.Provider
	if !options.LexicalOnly {
		provider, err = embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
		if err != nil {
			return search.Response{}, err
		}
	}
	return (&search.Searcher{Store: storage, Embedder: provider, SemanticThreshold: options.SemanticThreshold}).Search(ctx, query, search.Filters{
		Harnesses: options.Harnesses, Path: options.Path, Roles: options.Roles,
		Since: options.Since, Before: options.Before, Limit: options.Limit,
	})
}

func (a *App) runSessionStatus(ctx context.Context, cfg config.Config, args []string, recall bool) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	addSessionCommonFlags(flags, &cfg)
	if recall {
		addMemoryCommonFlags(flags, &cfg)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("status does not accept positional arguments")
	}
	storage, err := store.Open(ctx, cfg.Index)
	if err != nil {
		return err
	}
	defer storage.Close()
	embedder, err := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
	if err != nil {
		return err
	}
	stats, err := storage.Stats(ctx, embedder.Fingerprint())
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Index:       %s\nUpdated:     %s\nSources:     %d\nSessions:    %d\nChunks:      %d\nEmbeddings:  %d\nPending:     %d\nParse errors: %d\nModel:       %s\nDimensions:  %d\n",
		storage.Path(), emptyDash(stats.LastIndexedAt), stats.Sources, stats.Sessions,
		stats.Chunks, stats.Embeddings, stats.PendingEmbeddings, stats.ParserErrors,
		cfg.Embedding.Model, stats.EmbeddingDims)
	return nil
}

func (a *App) runSessionDoctor(ctx context.Context, cfg config.Config, configPath string, args []string, recall bool) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	addSessionCommonFlags(flags, &cfg)
	if recall {
		addMemoryCommonFlags(flags, &cfg)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("doctor does not accept positional arguments")
	}
	fmt.Fprintln(a.Stdout, "Config:", configPath)
	fmt.Fprintln(a.Stdout, "Index: ", cfg.Index)
	selected, err := selectHarnesses(cfg, "")
	if err != nil {
		return err
	}
	adapters, closeAdapters, err := makeAdapters(ctx, cfg, selected)
	if err != nil {
		return err
	}
	defer closeAdapters()
	failures := 0
	for _, sourceAdapter := range adapters {
		sources, err := sourceAdapter.Discover(ctx)
		if err != nil {
			fmt.Fprintf(a.Stdout, "[FAIL] %-8s %v\n", sourceAdapter.Name(), err)
			failures++
			continue
		}
		fmt.Fprintf(a.Stdout, "[OK]   %-8s %d sources\n", sourceAdapter.Name(), len(sources))
	}
	storage, err := store.Open(ctx, cfg.Index)
	if err != nil {
		fmt.Fprintln(a.Stdout, "[FAIL] index   ", err)
		failures++
	} else {
		defer storage.Close()
		fmt.Fprintln(a.Stdout, "[OK]   index    schema and permissions")
		errorsFound, err := storage.ParserErrors(ctx)
		if err != nil {
			fmt.Fprintln(a.Stdout, "[FAIL] errors  ", err)
			failures++
		} else {
			for _, message := range errorsFound {
				fmt.Fprintln(a.Stdout, "[WARN] parser  ", message)
			}
		}
	}
	embedder, err := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
	if err != nil {
		fmt.Fprintln(a.Stdout, "[FAIL] embedder ", err)
		failures++
	} else if vectors, err := embedder.Embed(ctx, []string{"agent session search health check"}); err != nil {
		fmt.Fprintln(a.Stdout, "[FAIL] embedder ", err)
		failures++
	} else {
		fmt.Fprintf(a.Stdout, "[OK]   embedder %s (%d dimensions)\n", embedder.Model(), len(vectors[0]))
	}
	if failures > 0 {
		return fmt.Errorf("doctor found %d failure(s)", failures)
	}
	return nil
}

func addSessionCommonFlags(flags *flag.FlagSet, cfg *config.Config) {
	flags.StringVar(&cfg.Index, "index", cfg.Index, "index database path")
	flags.StringVar(&cfg.Embedding.URL, "embedding-url", cfg.Embedding.URL, "Ollama base URL")
	flags.StringVar(&cfg.Embedding.Model, "embedding-model", cfg.Embedding.Model, "Ollama embedding model")
}

func addMemoryCommonFlags(flags *flag.FlagSet, cfg *config.Config) {
	flags.StringVar(&cfg.MemoryDirectory, "memory-directory", cfg.MemoryDirectory, "canonical memory directory")
	flags.StringVar(&cfg.MemoryIndex, "memory-index", cfg.MemoryIndex, "local memory index path")
}

func makeAdapters(ctx context.Context, cfg config.Config, selected []string) ([]model.Adapter, func(), error) {
	var values []model.Adapter
	var closers []model.AdapterCloser
	for _, name := range selected {
		var value model.Adapter
		switch name {
		case "pi":
			value = &adapter.Pi{Root: cfg.Sources.Pi}
		case "codex":
			value = &adapter.Codex{Root: cfg.Sources.Codex}
		case "opencode":
			value = &adapter.OpenCode{DBPath: cfg.Sources.OpenCode}
		case "claude":
			value = &adapter.Claude{Root: cfg.Sources.Claude}
		default:
			return nil, func() {}, fmt.Errorf("unsupported harness %q", name)
		}
		values = append(values, value)
		if closer, ok := value.(model.AdapterCloser); ok {
			closers = append(closers, closer)
		}
	}
	closeAll := func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}
	_ = ctx
	return values, closeAll, nil
}

func selectHarnesses(cfg config.Config, requested string) ([]string, error) {
	if requested != "" {
		return parseHarnessList(requested)
	}
	var values []string
	for _, name := range []string{"pi", "codex", "opencode", "claude"} {
		if cfg.Harnesses[name] {
			values = append(values, name)
		}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("no harnesses are enabled")
	}
	return values, nil
}

func parseHarnessList(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	allowed := map[string]bool{"pi": true, "codex": true, "opencode": true, "claude": true}
	seen := map[string]bool{}
	var result []string
	for _, item := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(item))
		if !allowed[name] {
			return nil, fmt.Errorf("unsupported harness %q", name)
		}
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result, nil
}

func parseRoles(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	allowed := map[string]bool{"user": true, "assistant": true}
	var result []string
	for _, item := range strings.Split(value, ",") {
		role := strings.ToLower(strings.TrimSpace(item))
		if !allowed[role] {
			return nil, fmt.Errorf("unsupported role %q", role)
		}
		result = append(result, role)
	}
	return result, nil
}

func parseDate(value string, endOfDay bool) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD or RFC3339")
	}
	if endOfDay {
		parsed = parsed.Add(24*time.Hour - time.Millisecond)
	}
	return parsed, nil
}

func validateSearchValues(limit int, threshold float64) error {
	if limit < 1 || limit > 100 {
		return fmt.Errorf("limit must be between 1 and 100")
	}
	if math.IsNaN(threshold) || threshold < 0 || threshold > 1 {
		return fmt.Errorf("semantic threshold must be between 0 and 1")
	}
	return nil
}

func (a *App) printSessionResults(results []search.Result, explain bool) {
	if len(results) == 0 {
		fmt.Fprintln(a.Stdout, "No matching sessions.")
		return
	}
	for _, result := range results {
		date := result.UpdatedAt.Format("2006-01-02")
		if result.UpdatedAt.IsZero() {
			date = "unknown date"
		}
		fmt.Fprintf(a.Stdout, "%d. [%s] %s\n   %s\n", result.CombinedRank, result.Harness, result.Name, date)
		if result.CWD != "" {
			fmt.Fprintln(a.Stdout, "   "+result.CWD)
		}
		fmt.Fprintln(a.Stdout, "   Session: "+result.SessionID)
		if result.ParentID != "" {
			fmt.Fprintln(a.Stdout, "   Parent:  "+result.ParentID)
		}
		if result.EntryID != "" {
			fmt.Fprintln(a.Stdout, "   Entry:   "+result.EntryID)
		}
		if result.Excerpt != "" {
			fmt.Fprintf(a.Stdout, "\n   %s\n", result.Excerpt)
		}
		if explain {
			parts := []string{}
			if result.Ranks.Exact > 0 {
				parts = append(parts, "exact="+strconv.Itoa(result.Ranks.Exact))
			}
			if result.Ranks.Lexical > 0 {
				parts = append(parts, "lexical="+strconv.Itoa(result.Ranks.Lexical))
			}
			if result.Ranks.Fuzzy > 0 {
				parts = append(parts, "fuzzy="+strconv.Itoa(result.Ranks.Fuzzy))
			}
			if result.Ranks.Semantic > 0 {
				semantic := "semantic=" + strconv.Itoa(result.Ranks.Semantic)
				semantic += " (similarity=" + strconv.FormatFloat(result.SemanticSimilarity, 'f', 3, 64) + ")"
				parts = append(parts, semantic)
			}
			fmt.Fprintln(a.Stdout, "\n   Ranks: "+strings.Join(parts, ", "))
		}
		fmt.Fprintf(a.Stdout, "\n   Resume:\n   %s\n\n", result.Resume)
	}
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func (a *App) printAgentUsage() {
	commands := []string{"index", "search", "status", "doctor", "version"}
	sort.Strings(commands)
	fmt.Fprintln(a.Stdout, "Search local coding-agent sessions.")
	fmt.Fprintln(a.Stdout, "\nUsage:\n  agent-sessions <command> [options]")
	fmt.Fprintln(a.Stdout, "\nCommands:")
	for _, command := range commands {
		fmt.Fprintln(a.Stdout, "  "+command)
	}
	fmt.Fprintln(a.Stdout, "\nExamples:")
	fmt.Fprintln(a.Stdout, "  agent-sessions index")
	fmt.Fprintln(a.Stdout, `  agent-sessions search --path ~/projects/qr-codes "QR codes without assets"`)
	fmt.Fprintln(a.Stdout, "  agent-sessions status")
}

func removeSQLite(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
