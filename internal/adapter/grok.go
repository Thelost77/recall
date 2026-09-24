package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/model"
)

type Grok struct {
	Root string
}

func (g *Grok) Name() string { return "grok" }

func (g *Grok) Discover(ctx context.Context) ([]model.Source, error) {
	if g.Root == "" {
		return nil, nil
	}
	if _, err := os.Stat(g.Root); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s source root %s does not exist", g.Name(), g.Root)
	} else if err != nil {
		return nil, fmt.Errorf("inspect %s source root %s: %w", g.Name(), g.Root, err)
	}

	var sources []model.Source
	err := filepath.WalkDir(g.Root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "updates.jsonl" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		sessionDir := filepath.Dir(path)
		sources = append(sources, model.Source{
			Key:     stableKey("grok", path),
			Harness: "grok",
			Path:    path,
			Version: grokSourceVersion(info, sessionDir),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover grok sessions: %w", err)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func (g *Grok) Parse(_ context.Context, source model.Source) (model.ParsedSource, error) {
	sessionDir := filepath.Dir(source.Path)
	summary, err := readGrokSummary(sessionDir)
	if err != nil {
		return model.ParsedSource{}, fmt.Errorf("parse Grok session %s: %w", source.Path, err)
	}

	sessionID := summary.Info.ID
	if sessionID == "" {
		sessionID = filepath.Base(sessionDir)
	}
	if sessionID == "" || sessionID == "." {
		return model.ParsedSource{}, fmt.Errorf("parse Grok session %s: missing session ID", source.Path)
	}

	var firstUser string
	var started, updated time.Time
	var entries []model.Entry
	ordinal := 0
	updateBounds(&started, &updated, parseISO(summary.CreatedAt))
	updateBounds(&started, &updated, parseISO(summary.UpdatedAt))

	warnings, err := decodeJSONLinesTolerant(source.Path, func(raw json.RawMessage) error {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		params, _ := value["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		if update == nil {
			return nil
		}
		var role string
		switch stringValue(update, "sessionUpdate") {
		case "user_message_chunk":
			role = "user"
		case "agent_message_chunk":
			role = "assistant"
		default:
			return nil
		}
		text := grokContentText(update)
		if role == "user" {
			text = grokUserText(text)
		}
		if text == "" {
			return nil
		}
		if role == "user" && firstUser == "" {
			firstUser = text
		}
		meta, _ := params["_meta"].(map[string]any)
		timestamp := grokEventTime(value, meta)
		updateBounds(&started, &updated, timestamp)
		nativeID := stringValue(meta, "eventId")
		if nativeID == "" {
			ordinal++
			nativeID = fmt.Sprintf("message-%d", ordinal)
		}
		entries = append(entries, model.Entry{
			NativeID: nativeID, Role: role, Kind: "message",
			Timestamp: timestamp, Text: text,
		})
		return nil
	})
	if err != nil {
		return model.ParsedSource{}, fmt.Errorf("parse Grok session %s: %w", source.Path, err)
	}

	name := strings.TrimSpace(summary.GeneratedTitle)
	if name == "" {
		name = strings.TrimSpace(summary.SessionSummary)
	}
	if name == "" {
		name = firstLineTitle(firstUser)
	}
	var gitRemote string
	for _, remote := range summary.GitRemotes {
		if strings.TrimSpace(remote) != "" {
			gitRemote = remote
			break
		}
	}

	sessionKey := stableKey("grok", sessionID)
	for index := range entries {
		entry := &entries[index]
		entry.SessionKey = sessionKey
		entry.Key = stableKey("grok", sessionID, entry.NativeID, entry.Kind)
	}
	return model.ParsedSource{
		Sessions: []model.Session{{
			Key: sessionKey, Harness: "grok", NativeID: sessionID,
			ParentID: summary.ParentSessionID, Name: name,
			CWD:       grokWorkingDirectory(sessionDir, summary.Info.CWD),
			SourceKey: source.Key, SourcePath: source.Path,
			StartedAt: started, UpdatedAt: updated,
			GitBranch: summary.HeadBranch, GitRemote: gitRemote,
		}},
		Entries: entries, Warnings: warnings,
	}, nil
}

type grokSummary struct {
	Info struct {
		ID  string `json:"id"`
		CWD string `json:"cwd"`
	} `json:"info"`
	SessionSummary  string   `json:"session_summary"`
	GeneratedTitle  string   `json:"generated_title"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	HeadBranch      string   `json:"head_branch"`
	GitRemotes      []string `json:"git_remotes"`
	ParentSessionID string   `json:"parent_session_id"`
}

func readGrokSummary(sessionDir string) (grokSummary, error) {
	path := filepath.Join(sessionDir, "summary.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return grokSummary{}, nil
	}
	if err != nil {
		return grokSummary{}, err
	}
	var summary grokSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return grokSummary{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return summary, nil
}

func grokSourceVersion(updates fs.FileInfo, sessionDir string) string {
	version := fmt.Sprintf("grok-v1:%d:%d", updates.Size(), updates.ModTime().UnixNano())
	summary, err := os.Stat(filepath.Join(sessionDir, "summary.json"))
	if err != nil {
		return version
	}
	return version + fmt.Sprintf(":%d:%d", summary.Size(), summary.ModTime().UnixNano())
}

func grokWorkingDirectory(sessionDir, summaryCWD string) string {
	if strings.TrimSpace(summaryCWD) != "" {
		return summaryCWD
	}
	group := filepath.Dir(sessionDir)
	if data, err := os.ReadFile(filepath.Join(group, ".cwd")); err == nil {
		if text := strings.TrimSpace(string(data)); text != "" {
			return text
		}
	}
	decoded, err := url.PathUnescape(filepath.Base(group))
	if err != nil {
		return ""
	}
	return decoded
}

func grokContentText(update map[string]any) string {
	switch content := update["content"].(type) {
	case string:
		return strings.TrimSpace(content)
	case map[string]any:
		return strings.TrimSpace(stringValue(content, "text"))
	default:
		return ""
	}
}

func grokUserText(text string) string {
	const open = "<user_query>"
	const closeTag = "</user_query>"
	start := strings.Index(text, open)
	if start < 0 {
		return strings.TrimSpace(text)
	}
	rest := text[start+len(open):]
	if end := strings.Index(rest, closeTag); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// grokEventTime reads an ACP update clock. The line timestamp is Unix seconds.
// agentTimestampMs is Unix milliseconds and wins when both are present.
func grokEventTime(value, meta map[string]any) time.Time {
	if ms := jsonInt64(meta["agentTimestampMs"]); ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	if seconds := jsonInt64(value["timestamp"]); seconds > 0 {
		return time.Unix(seconds, 0).UTC()
	}
	return time.Time{}
}

func jsonInt64(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int64:
		return number
	default:
		return 0
	}
}
