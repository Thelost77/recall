package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Thelost77/agent-sessions/internal/config"
	"github.com/Thelost77/agent-sessions/internal/embed"
	"github.com/Thelost77/agent-sessions/internal/memory"
	"github.com/Thelost77/agent-sessions/internal/project"
	"github.com/Thelost77/agent-sessions/internal/search"
	"github.com/Thelost77/agent-sessions/internal/store"
)

type recallSource int

const (
	recallBoth recallSource = iota
	recallSessions
	recallMemories
)

type recallSearchOutput struct {
	Query    string           `json:"query"`
	Memories memory.Response  `json:"memories"`
	Sessions search.Response  `json:"sessions"`
	Warnings []memory.Warning `json:"warnings"`
}

func (a *App) runRecall(ctx context.Context, cfg config.Config, configPath string, args []string) error {
	if len(args) == 0 {
		a.printRecallUsage()
		return nil
	}
	if args[0] == "--" {
		return a.runRecallSearch(ctx, cfg, args[1:], recallBoth)
	}
	switch args[0] {
	case "session":
		return a.runRecallSearch(ctx, cfg, args[1:], recallSessions)
	case "memory":
		return a.runRecallSearch(ctx, cfg, args[1:], recallMemories)
	case "search":
		return a.runRecallSearch(ctx, cfg, args[1:], recallBoth)
	case "remember":
		return a.runRemember(ctx, cfg, args[1:])
	case "list":
		return a.runMemoryList(ctx, cfg, args[1:])
	case "show":
		return a.runMemoryShow(cfg, args[1:])
	case "edit":
		return a.runMemoryEdit(ctx, cfg, args[1:])
	case "forget":
		return a.runMemoryArchive(ctx, cfg, args[1:], false)
	case "restore":
		return a.runMemoryArchive(ctx, cfg, args[1:], true)
	case "history":
		return a.runMemoryHistory(cfg, args[1:])
	case "index":
		return a.runRecallIndex(ctx, cfg, args[1:])
	case "status":
		return a.runRecallStatus(ctx, cfg, args[1:])
	case "doctor":
		return a.runRecallDoctor(ctx, cfg, configPath, args[1:])
	case "version", "--version", "-version":
		fmt.Fprintln(a.Stdout, BuildVersion(a.Version))
		return nil
	case "help", "--help", "-h":
		a.printRecallUsage()
		return nil
	default:
		return a.runRecallSearch(ctx, cfg, args, recallBoth)
	}
}

func (a *App) runRecallSearch(ctx context.Context, cfg config.Config, args []string, forced recallSource) error {
	flags := flag.NewFlagSet("recall search", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	sessionsOnly := flags.Bool("s", false, "search sessions only")
	memoriesOnly := flags.Bool("m", false, "search memories only")
	path := flags.String("path", "", "exact session directory or parent directory")
	harnesses := flags.String("harness", "", "comma-separated harness names")
	roles := flags.String("role", "", "comma-separated roles")
	since := flags.String("since", "", "minimum session date")
	before := flags.String("before", "", "maximum session date")
	limit := flags.Int("limit", search.DefaultLimit, "maximum results per source")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	explain := flags.Bool("explain", false, "show retrieval channel ranks")
	lexicalOnly := flags.Bool("lexical-only", false, "skip semantic retrieval")
	semanticThreshold := flags.Float64("semantic-threshold", cfg.Embedding.Threshold, "minimum semantic cosine similarity")
	includeArchived := flags.Bool("include-archived", false, "include archived memories")
	allProjects := flags.Bool("all-projects", false, "search every project")
	globalOnly := flags.Bool("global", false, "search global memories only")
	projectPath := flags.String("project", "", "select a Git project")
	addSessionCommonFlags(flags, &cfg)
	addMemoryCommonFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *sessionsOnly && *memoriesOnly {
		return fmt.Errorf("-s and -m are mutually exclusive")
	}
	selected := forced
	if *sessionsOnly {
		if forced == recallMemories {
			return fmt.Errorf("memory search cannot use -s")
		}
		selected = recallSessions
	}
	if *memoriesOnly {
		if forced == recallSessions {
			return fmt.Errorf("session search cannot use -m")
		}
		selected = recallMemories
	}
	visited := map[string]bool{}
	flags.Visit(func(item *flag.Flag) { visited[item.Name] = true })
	if selected == recallSessions {
		for _, name := range []string{"include-archived", "global", "project", "memory-directory", "memory-index"} {
			if visited[name] {
				return fmt.Errorf("--%s does not apply to session search", name)
			}
		}
	}
	if selected == recallMemories {
		for _, name := range []string{"path", "harness", "role", "since", "before", "index"} {
			if visited[name] {
				return fmt.Errorf("--%s does not apply to memory search", name)
			}
		}
	}
	if *globalOnly && (*allProjects || *projectPath != "") {
		return fmt.Errorf("--global is mutually exclusive with --project and --all-projects")
	}
	if *allProjects && *projectPath != "" {
		return fmt.Errorf("--project and --all-projects are mutually exclusive")
	}
	query := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if query == "" {
		return fmt.Errorf("search requires a query")
	}
	if err := validateSearchValues(*limit, *semanticThreshold); err != nil {
		return err
	}

	output := recallSearchOutput{
		Query: query, Memories: memory.Response{Results: []memory.Result{}},
		Sessions: search.Response{Results: []search.Result{}}, Warnings: []memory.Warning{},
	}
	if selected != recallSessions {
		filters, err := a.memoryFilters(*projectPath, *globalOnly, *allProjects, *includeArchived, *limit)
		if err != nil {
			return err
		}
		response, warnings, err := searchMemories(ctx, cfg, query, filters, *lexicalOnly, *semanticThreshold)
		if err != nil {
			return err
		}
		output.Memories = response
		output.Warnings = append(output.Warnings, warnings...)
		if response.SemanticError != "" {
			output.Warnings = append(output.Warnings, memory.Warning{Type: "semantic_search", Message: response.SemanticError})
		}
	}
	if selected != recallMemories {
		selectedHarnesses, err := parseHarnessList(*harnesses)
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
		sessionPath := *path
		if sessionPath == "" && !*allProjects {
			if cwd, err := a.Getwd(); err == nil {
				if identity, err := project.Resolve(cwd); err == nil {
					sessionPath = identity.Root
				}
			}
		}
		if sessionPath != "" {
			absolute, err := filepath.Abs(sessionPath)
			if err != nil {
				return err
			}
			sessionPath = filepath.Clean(absolute)
		}
		response, err := searchSessions(ctx, cfg, query, sessionSearchOptions{
			Path: sessionPath, Harnesses: selectedHarnesses, Roles: roleValues,
			Since: sinceTime, Before: beforeTime, Limit: *limit,
			LexicalOnly: *lexicalOnly, SemanticThreshold: *semanticThreshold,
		})
		if err != nil {
			return err
		}
		if response.Results == nil {
			response.Results = []search.Result{}
		}
		output.Sessions = response
		if response.SemanticError != "" {
			output.Warnings = append(output.Warnings, memory.Warning{Type: "semantic_search", Message: "session search: " + response.SemanticError})
		}
	}
	if *jsonOutput {
		encoder := json.NewEncoder(a.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(output)
	}
	for _, warning := range output.Warnings {
		a.printWarning(warning)
	}
	if selected != recallSessions {
		fmt.Fprintln(a.Stdout, "MEMORIES")
		fmt.Fprintln(a.Stdout)
		a.printMemoryResults(output.Memories.Results, *explain)
	}
	if selected == recallBoth {
		fmt.Fprintln(a.Stdout)
	}
	if selected != recallMemories {
		fmt.Fprintln(a.Stdout, "SESSIONS")
		fmt.Fprintln(a.Stdout)
		a.printSessionResults(output.Sessions.Results, *explain)
	}
	return nil
}

func searchMemories(ctx context.Context, cfg config.Config, query string, filters memory.Filters, lexicalOnly bool, threshold float64) (memory.Response, []memory.Warning, error) {
	if err := memory.EnsureIndexOutsideCanonical(cfg.MemoryDirectory, cfg.MemoryIndex); err != nil {
		return memory.Response{}, nil, err
	}
	index, err := memory.OpenIndex(ctx, cfg.MemoryIndex)
	if err != nil {
		return memory.Response{}, nil, err
	}
	defer index.Close()
	var provider embed.Provider
	if !lexicalOnly {
		provider, err = embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
		if err != nil {
			return memory.Response{}, nil, err
		}
	}
	refresh, err := memory.Refresh(ctx, memory.FileStore{Directory: cfg.MemoryDirectory}, index, provider)
	if err != nil {
		return memory.Response{}, nil, err
	}
	warnings := append([]memory.Warning{}, refresh.Warnings...)
	if refresh.EmbeddingError != nil {
		warnings = append(warnings, memory.Warning{Type: "semantic_index", Message: "memory embeddings are pending: " + refresh.EmbeddingError.Error()})
	}
	response, err := (&memory.Searcher{Index: index, Embedder: provider, SemanticThreshold: threshold}).Search(ctx, query, filters)
	return response, warnings, err
}

func (a *App) memoryFilters(projectPath string, globalOnly, allProjects, includeArchived bool, limit int) (memory.Filters, error) {
	filters := memory.Filters{GlobalOnly: globalOnly, AllProjects: allProjects, IncludeArchived: includeArchived, Limit: limit}
	if globalOnly || allProjects {
		return filters, nil
	}
	path := projectPath
	if path == "" {
		var err error
		path, err = a.Getwd()
		if err != nil {
			return filters, err
		}
	}
	identity, err := project.Resolve(path)
	if err != nil {
		if projectPath != "" {
			return filters, err
		}
		filters.GlobalOnly = true
		return filters, nil
	}
	filters.ProjectKey = identity.Key
	return filters, nil
}

func (a *App) runRemember(ctx context.Context, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("remember", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	projectPath := flags.String("project", "", "Git project path")
	global := flags.Bool("global", false, "create a global memory")
	source := flags.String("source", "", "source reference")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	addMemoryWriteFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *global && *projectPath != "" {
		return fmt.Errorf("--global and --project are mutually exclusive")
	}
	content, err := a.memoryContent(flags.Args())
	if err != nil {
		return err
	}
	if err := prepareMemoryConfig(cfg); err != nil {
		return err
	}
	scope := memory.ScopeProject
	var key, remote, name string
	if *global {
		scope = memory.ScopeGlobal
	} else {
		path := *projectPath
		if path == "" {
			path, err = a.Getwd()
			if err != nil {
				return err
			}
		}
		identity, resolveErr := project.Resolve(path)
		if resolveErr != nil {
			return fmt.Errorf("project memory requires a Git repository; use --global outside Git: %w", resolveErr)
		}
		key, remote, name = identity.Key, identity.Remote, identity.Name
	}
	record, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Create(scope, content, strings.TrimSpace(*source), key, remote, name)
	if err != nil {
		return err
	}
	a.refreshAfterWrite(ctx, cfg)
	if *jsonOutput {
		return writeJSON(a.Stdout, record)
	}
	fmt.Fprintf(a.Stdout, "Remembered %s [%s]\n", record.ID, record.Scope)
	return nil
}

func (a *App) runMemoryEdit(ctx context.Context, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("edit", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	addMemoryWriteFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() == 0 {
		return fmt.Errorf("edit requires a memory ID and content")
	}
	id := flags.Arg(0)
	content, err := a.memoryContent(flags.Args()[1:])
	if err != nil {
		return err
	}
	if err := prepareMemoryConfig(cfg); err != nil {
		return err
	}
	record, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Edit(id, content)
	if err != nil {
		return err
	}
	a.refreshAfterWrite(ctx, cfg)
	if *jsonOutput {
		return writeJSON(a.Stdout, record)
	}
	fmt.Fprintf(a.Stdout, "Edited %s (revision %d)\n", record.ID, record.CurrentRevision)
	return nil
}

func (a *App) runMemoryArchive(ctx context.Context, cfg config.Config, args []string, restore bool) error {
	name := "forget"
	if restore {
		name = "restore"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	addMemoryWriteFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%s requires one memory ID", name)
	}
	if err := prepareMemoryConfig(cfg); err != nil {
		return err
	}
	files := memory.FileStore{Directory: cfg.MemoryDirectory}
	var record memory.Record
	var err error
	if restore {
		record, err = files.Restore(flags.Arg(0))
	} else {
		record, err = files.Forget(flags.Arg(0))
	}
	if err != nil {
		return err
	}
	a.refreshAfterWrite(ctx, cfg)
	if *jsonOutput {
		return writeJSON(a.Stdout, record)
	}
	past := "Archived"
	if restore {
		past = "Restored"
	}
	fmt.Fprintf(a.Stdout, "%s %s\n", past, record.ID)
	return nil
}

func (a *App) runMemoryShow(cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("show", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&cfg.MemoryDirectory, "memory-directory", cfg.MemoryDirectory, "canonical memory directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("show requires one memory ID")
	}
	record, _, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Load(flags.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, record)
	}
	a.printMemoryRecord(record)
	return nil
}

func (a *App) runMemoryHistory(cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("history", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&cfg.MemoryDirectory, "memory-directory", cfg.MemoryDirectory, "canonical memory directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("history requires one memory ID")
	}
	record, _, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Load(flags.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, struct {
			ID        string            `json:"id"`
			Revisions []memory.Revision `json:"revisions"`
		}{ID: record.ID, Revisions: record.Revisions})
	}
	fmt.Fprintf(a.Stdout, "%s history\n\n", record.ID)
	for _, revision := range record.Revisions {
		fmt.Fprintf(a.Stdout, "Revision %d  %s\n%s\n\n", revision.Revision, revision.CreatedAt.Format(time.RFC3339), revision.Content)
	}
	return nil
}

func (a *App) runMemoryList(_ context.Context, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	projectPath := flags.String("project", "", "select a Git project")
	globalOnly := flags.Bool("global", false, "list global memories only")
	allProjects := flags.Bool("all-projects", false, "list every project")
	includeArchived := flags.Bool("include-archived", false, "include archived memories")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&cfg.MemoryDirectory, "memory-directory", cfg.MemoryDirectory, "canonical memory directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("list does not accept positional arguments")
	}
	if *globalOnly && (*allProjects || *projectPath != "") {
		return fmt.Errorf("--global is mutually exclusive with --project and --all-projects")
	}
	if *allProjects && *projectPath != "" {
		return fmt.Errorf("--project and --all-projects are mutually exclusive")
	}
	filters, err := a.memoryFilters(*projectPath, *globalOnly, *allProjects, *includeArchived, 100)
	if err != nil {
		return err
	}
	scan, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Scan()
	if err != nil {
		return err
	}
	records := make([]memory.Record, 0, len(scan.Records))
	for _, item := range scan.Records {
		if memoryRecordMatches(item.Record, filters) {
			records = append(records, item.Record)
		}
	}
	sort.SliceStable(records, func(left, right int) bool { return records[left].UpdatedAt.After(records[right].UpdatedAt) })
	if *jsonOutput {
		return writeJSON(a.Stdout, struct {
			Results  []memory.Record  `json:"results"`
			Warnings []memory.Warning `json:"warnings"`
		}{Results: records, Warnings: scan.Warnings})
	}
	for _, warning := range scan.Warnings {
		a.printWarning(warning)
	}
	if len(records) == 0 {
		fmt.Fprintln(a.Stdout, "No memories.")
		return nil
	}
	for position, record := range records {
		state := ""
		if record.ArchivedAt != nil {
			state = " archived"
		}
		fmt.Fprintf(a.Stdout, "%d. %s [%s%s]\n   %s\n", position+1, record.ID, record.Scope, state, oneLine(record.CurrentContent()))
	}
	return nil
}

func memoryRecordMatches(record memory.Record, filters memory.Filters) bool {
	if !filters.IncludeArchived && record.ArchivedAt != nil {
		return false
	}
	if filters.AllProjects {
		return true
	}
	if filters.GlobalOnly || filters.ProjectKey == "" {
		return record.Scope == memory.ScopeGlobal
	}
	return record.Scope == memory.ScopeGlobal || (record.Scope == memory.ScopeProject && record.ProjectKey == filters.ProjectKey)
}

func addMemoryWriteFlags(flags *flag.FlagSet, cfg *config.Config) {
	addMemoryCommonFlags(flags, cfg)
	flags.StringVar(&cfg.Embedding.URL, "embedding-url", cfg.Embedding.URL, "Ollama base URL")
	flags.StringVar(&cfg.Embedding.Model, "embedding-model", cfg.Embedding.Model, "Ollama embedding model")
}

func prepareMemoryConfig(cfg config.Config) error {
	if err := memory.EnsureIndexOutsideCanonical(cfg.MemoryDirectory, cfg.MemoryIndex); err != nil {
		return err
	}
	_, err := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
	return err
}

func (a *App) refreshAfterWrite(ctx context.Context, cfg config.Config) {
	index, err := memory.OpenIndex(ctx, cfg.MemoryIndex)
	if err != nil {
		fmt.Fprintln(a.Stderr, "warning: memory was saved but the local index could not be updated:", err)
		return
	}
	defer index.Close()
	provider, err := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
	if err != nil {
		fmt.Fprintln(a.Stderr, "warning: memory was saved but semantic indexing is unavailable:", err)
		return
	}
	result, err := memory.Refresh(ctx, memory.FileStore{Directory: cfg.MemoryDirectory}, index, provider)
	if err != nil {
		fmt.Fprintln(a.Stderr, "warning: memory was saved but the local index could not be updated:", err)
		return
	}
	for _, warning := range result.Warnings {
		a.printWarning(warning)
	}
	if result.EmbeddingError != nil {
		fmt.Fprintln(a.Stderr, "warning: memory was saved; its embedding remains pending:", result.EmbeddingError)
	}
}

func (a *App) memoryContent(arguments []string) (string, error) {
	if content := strings.TrimSpace(strings.Join(arguments, " ")); content != "" {
		return content, nil
	}
	if file, ok := a.Stdin.(*os.File); ok {
		info, err := file.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("memory content is required as arguments or piped standard input")
		}
	}
	payload, err := io.ReadAll(a.Stdin)
	if err != nil {
		return "", fmt.Errorf("read memory content from standard input: %w", err)
	}
	content := strings.TrimSpace(string(payload))
	if content == "" {
		return "", fmt.Errorf("memory content is required as arguments or piped standard input")
	}
	return content, nil
}

func (a *App) runRecallIndex(ctx context.Context, cfg config.Config, args []string) error {
	sessionErr := a.runSessionIndex(ctx, cfg, args, true)
	flags := flag.NewFlagSet("index", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	rebuild := flags.Bool("rebuild", false, "rebuild both derived indexes")
	reembed := flags.Bool("reembed", false, "regenerate every embedding")
	quiet := flags.Bool("quiet", false, "suppress progress messages")
	_ = flags.String("harness", "", "comma-separated harness names")
	addSessionCommonFlags(flags, &cfg)
	addMemoryCommonFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return sessionErr
		}
		return errors.Join(sessionErr, err)
	}
	if err := memory.EnsureIndexOutsideCanonical(cfg.MemoryDirectory, cfg.MemoryIndex); err != nil {
		return errors.Join(sessionErr, err)
	}
	if *rebuild {
		if err := removeSQLite(cfg.MemoryIndex); err != nil {
			return errors.Join(sessionErr, fmt.Errorf("remove old memory index: %w", err))
		}
	}
	index, err := memory.OpenIndex(ctx, cfg.MemoryIndex)
	if err != nil {
		return errors.Join(sessionErr, err)
	}
	defer index.Close()
	if *reembed {
		if err := index.ClearEmbeddings(ctx); err != nil {
			return errors.Join(sessionErr, err)
		}
	}
	provider, err := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
	if err != nil {
		return errors.Join(sessionErr, err)
	}
	result, memoryErr := memory.Refresh(ctx, memory.FileStore{Directory: cfg.MemoryDirectory}, index, provider)
	if !*quiet {
		fmt.Fprintf(a.Stdout, "Memory records: %d\nMemory updated: %d\nMemory unchanged: %d\nMemory deleted: %d\nMemory embedded: %d\n", result.Discovered, result.Updated, result.Unchanged, result.Deleted, result.Embedded)
	}
	for _, warning := range result.Warnings {
		a.printWarning(warning)
	}
	if result.EmbeddingError != nil {
		fmt.Fprintln(a.Stderr, "warning: semantic memory indexing is incomplete:", result.EmbeddingError)
	}
	return errors.Join(sessionErr, memoryErr)
}

func (a *App) runRecallStatus(ctx context.Context, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	addSessionCommonFlags(flags, &cfg)
	addMemoryCommonFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("status does not accept positional arguments")
	}
	provider, err := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
	if err != nil {
		return err
	}
	sessionStore, err := store.Open(ctx, cfg.Index)
	if err != nil {
		return err
	}
	defer sessionStore.Close()
	sessionStats, err := sessionStore.Stats(ctx, provider.Fingerprint())
	if err != nil {
		return err
	}
	scan, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Scan()
	if err != nil {
		return err
	}
	active, archived, invalid, conflicts := 0, 0, 0, 0
	for _, item := range scan.Records {
		if item.Record.ArchivedAt == nil {
			active++
		} else {
			archived++
		}
	}
	for _, warning := range scan.Warnings {
		if warning.Type == "invalid_record" {
			invalid++
		} else if warning.Type == "syncthing_conflict" {
			conflicts++
		}
	}
	if err := memory.EnsureIndexOutsideCanonical(cfg.MemoryDirectory, cfg.MemoryIndex); err != nil {
		return err
	}
	memoryIndex, err := memory.OpenIndex(ctx, cfg.MemoryIndex)
	if err != nil {
		return err
	}
	defer memoryIndex.Close()
	if _, err := memory.Refresh(ctx, memory.FileStore{Directory: cfg.MemoryDirectory}, memoryIndex, nil); err != nil {
		return err
	}
	memoryStats, err := memoryIndex.Stats(ctx, provider.Fingerprint())
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "SESSION INDEX\nIndex:       %s\nUpdated:     %s\nSources:     %d\nSessions:    %d\nChunks:      %d\nEmbeddings:  %d\nPending:     %d\nParse errors: %d\n\n",
		cfg.Index, emptyDash(sessionStats.LastIndexedAt), sessionStats.Sources, sessionStats.Sessions,
		sessionStats.Chunks, sessionStats.Embeddings, sessionStats.PendingEmbeddings, sessionStats.ParserErrors)
	fmt.Fprintf(a.Stdout, "MEMORIES\nDirectory:   %s\nIndex:       %s\nRecords:     %d\nActive:      %d\nArchived:    %d\nEmbeddings:  %d\nPending:     %d\nInvalid:     %d\nConflicts:   %d\nDimensions:  %d\n",
		cfg.MemoryDirectory, cfg.MemoryIndex, len(scan.Records)+invalid, active, archived,
		memoryStats.Embeddings, memoryStats.PendingEmbeddings, invalid, conflicts, memoryStats.EmbeddingDims)
	for _, warning := range scan.Warnings {
		a.printWarning(warning)
	}
	return nil
}

func (a *App) runRecallDoctor(ctx context.Context, cfg config.Config, configPath string, args []string) error {
	sessionErr := a.runSessionDoctor(ctx, cfg, configPath, args, true)
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	addSessionCommonFlags(flags, &cfg)
	addMemoryCommonFlags(flags, &cfg)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return sessionErr
		}
		return errors.Join(sessionErr, err)
	}
	failures := 0
	fmt.Fprintln(a.Stdout, "Memory directory:", cfg.MemoryDirectory)
	if err := memory.EnsureIndexOutsideCanonical(cfg.MemoryDirectory, cfg.MemoryIndex); err != nil {
		fmt.Fprintln(a.Stdout, "[FAIL] memory paths", err)
		failures++
	} else {
		fmt.Fprintln(a.Stdout, "[OK]   memory paths canonical data and SQLite index are separate")
	}
	scan, err := (memory.FileStore{Directory: cfg.MemoryDirectory}).Scan()
	if err != nil {
		fmt.Fprintln(a.Stdout, "[FAIL] memories", err)
		failures++
	} else {
		fmt.Fprintf(a.Stdout, "[OK]   memories %d valid canonical records\n", len(scan.Records))
		for _, directory := range []string{cfg.MemoryDirectory, filepath.Join(cfg.MemoryDirectory, "records")} {
			if info, statErr := os.Stat(directory); statErr == nil && info.Mode().Perm()&0o077 != 0 {
				fmt.Fprintf(a.Stdout, "[WARN] permissions %s mode %04o is broader than 0700\n", directory, info.Mode().Perm())
			}
		}
		for _, item := range scan.Records {
			if info, statErr := os.Stat(item.Path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
				fmt.Fprintf(a.Stdout, "[WARN] permissions %s mode %04o is broader than 0600\n", item.Path, info.Mode().Perm())
			}
		}
		for _, warning := range scan.Warnings {
			label := "[WARN] conflict"
			if warning.Type == "invalid_record" {
				label = "[WARN] invalid "
			}
			fmt.Fprintln(a.Stdout, label, warning.MemoryID, warning.Message, warning.Path, strings.Join(warning.ConflictPaths, ", "))
		}
	}
	index, err := memory.OpenIndex(ctx, cfg.MemoryIndex)
	if err != nil {
		fmt.Fprintln(a.Stdout, "[FAIL] memory index", err)
		failures++
	} else {
		defer index.Close()
		fmt.Fprintln(a.Stdout, "[OK]   memory index schema and permissions")
		provider, providerErr := embed.New(cfg.Embedding.URL, cfg.Embedding.Model)
		if providerErr == nil {
			if stats, statsErr := index.Stats(ctx, provider.Fingerprint()); statsErr != nil {
				fmt.Fprintln(a.Stdout, "[FAIL] memory dimensions", statsErr)
				failures++
			} else {
				fmt.Fprintf(a.Stdout, "[OK]   memory dimensions %d\n", stats.EmbeddingDims)
			}
		}
	}
	if failures > 0 {
		return errors.Join(sessionErr, fmt.Errorf("doctor found %d memory failure(s)", failures))
	}
	return sessionErr
}

func (a *App) printMemoryResults(results []memory.Result, explain bool) {
	if len(results) == 0 {
		fmt.Fprintln(a.Stdout, "No matching memories.")
		return
	}
	for _, result := range results {
		state := result.Scope
		if result.ArchivedAt != nil {
			state += ", archived"
		}
		fmt.Fprintf(a.Stdout, "%d. %s [%s]\n   %s\n", result.CombinedRank, result.ID, state, oneLine(result.Content))
		if result.ProjectName != "" {
			fmt.Fprintf(a.Stdout, "   Project: %s\n", result.ProjectName)
		}
		if result.Source != "" {
			fmt.Fprintf(a.Stdout, "   Source: %s\n", result.Source)
		}
		if explain {
			var parts []string
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
				parts = append(parts, "semantic="+strconv.Itoa(result.Ranks.Semantic)+" (similarity="+strconv.FormatFloat(result.SemanticSimilarity, 'f', 3, 64)+")")
			}
			fmt.Fprintln(a.Stdout, "   Ranks: "+strings.Join(parts, ", "))
		}
		fmt.Fprintln(a.Stdout)
	}
}

func (a *App) printMemoryRecord(record memory.Record) {
	archived := "-"
	if record.ArchivedAt != nil {
		archived = record.ArchivedAt.Format(time.RFC3339)
	}
	fmt.Fprintf(a.Stdout, "ID:          %s\nScope:       %s\nProject:     %s\nProject key: %s\nRemote:      %s\nSource:      %s\nCreated:     %s\nUpdated:     %s\nArchived:    %s\nRevision:    %d\n\n%s\n",
		record.ID, record.Scope, emptyDash(record.ProjectName), emptyDash(record.ProjectKey), emptyDash(record.ProjectRemote),
		emptyDash(record.Source), record.CreatedAt.Format(time.RFC3339), record.UpdatedAt.Format(time.RFC3339),
		archived, record.CurrentRevision, record.CurrentContent())
}

func (a *App) printWarning(warning memory.Warning) {
	fmt.Fprintln(a.Stderr, "warning:", warning.Message)
	if warning.MemoryID != "" {
		fmt.Fprintln(a.Stderr, "  memory:   ", warning.MemoryID)
	}
	if warning.Path != "" {
		fmt.Fprintln(a.Stderr, "  canonical:", warning.Path)
	}
	for _, path := range warning.ConflictPaths {
		fmt.Fprintln(a.Stderr, "  conflict: ", path)
	}
}

func oneLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func (a *App) printRecallUsage() {
	fmt.Fprintln(a.Stdout, "Search local coding-agent sessions and explicit memories.")
	fmt.Fprintln(a.Stdout, "\nUsage:")
	fmt.Fprintln(a.Stdout, `  recall "query"`)
	fmt.Fprintln(a.Stdout, `  recall -s "query" | recall session "query"`)
	fmt.Fprintln(a.Stdout, `  recall -m "query" | recall memory "query"`)
	fmt.Fprintln(a.Stdout, "  recall <command> [options]")
	fmt.Fprintln(a.Stdout, "\nMemory commands:")
	fmt.Fprintln(a.Stdout, "  remember  list  show  edit  forget  restore  history")
	fmt.Fprintln(a.Stdout, "\nOther commands:")
	fmt.Fprintln(a.Stdout, "  index  status  doctor  version")
	fmt.Fprintln(a.Stdout, "\nUse -- before a query that starts with a command word.")
}
