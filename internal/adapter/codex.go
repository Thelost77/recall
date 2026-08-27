package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/model"
)

type Codex struct {
	Root string
}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Discover(ctx context.Context) ([]model.Source, error) {
	return fileSources(ctx, c.Name(), c.Root, nil)
}

func (c *Codex) Parse(_ context.Context, source model.Source) (model.ParsedSource, error) {
	var sessionID, cwd, firstUser, gitBranch, gitRemote string
	var started, updated time.Time
	var entries []model.Entry
	ordinal := 0

	warnings, err := decodeJSONLinesTolerant(source.Path, func(raw json.RawMessage) error {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		timestamp := parseISO(stringValue(value, "timestamp"))
		updateBounds(&started, &updated, timestamp)
		payload, _ := value["payload"].(map[string]any)

		switch stringValue(value, "type") {
		case "session_meta":
			sessionID = stringValue(payload, "id")
			if sessionID == "" {
				sessionID = stringValue(payload, "session_id")
			}
			cwd = stringValue(payload, "cwd")
			if git, ok := payload["git"].(map[string]any); ok {
				gitBranch = stringValue(git, "branch")
				gitRemote = stringValue(git, "repository_url")
			}
		case "response_item":
			if stringValue(payload, "type") != "message" {
				return nil
			}
			role := stringValue(payload, "role")
			if role != "user" && role != "assistant" {
				return nil
			}
			text := contentText(payload["content"], map[string]bool{
				"input_text": true, "output_text": true, "text": true,
			})
			if text == "" || (role == "user" && isCodexInjectedText(text)) {
				return nil
			}
			if role == "user" && firstUser == "" {
				firstUser = text
			}
			ordinal++
			nativeID := stringValue(payload, "id")
			if nativeID == "" {
				nativeID = fmt.Sprintf("message-%d", ordinal)
			}
			entries = append(entries, model.Entry{
				NativeID: nativeID, Role: role, Kind: "message",
				Timestamp: timestamp, Text: text,
			})
		case "compacted":
			text := stringValue(payload, "message")
			if strings.TrimSpace(text) == "" {
				return nil
			}
			nativeID := stringValue(payload, "window_id")
			if nativeID == "" {
				ordinal++
				nativeID = fmt.Sprintf("compaction-%d", ordinal)
			}
			entries = append(entries, model.Entry{
				NativeID: nativeID, Role: "assistant", Kind: "compaction",
				Timestamp: timestamp, Text: text,
			})
		}
		return nil
	})
	if err != nil {
		return model.ParsedSource{}, fmt.Errorf("parse Codex session %s: %w", source.Path, err)
	}
	if sessionID == "" {
		return model.ParsedSource{}, fmt.Errorf("parse Codex session %s: missing session metadata", source.Path)
	}

	sessionKey := stableKey("codex", sessionID)
	for index := range entries {
		entry := &entries[index]
		entry.SessionKey = sessionKey
		entry.Key = stableKey("codex", sessionID, entry.NativeID, entry.Kind)
	}
	return model.ParsedSource{
		Sessions: []model.Session{{
			Key: sessionKey, Harness: "codex", NativeID: sessionID,
			Name: firstLineTitle(firstUser), CWD: cwd,
			SourceKey: source.Key, SourcePath: source.Path,
			StartedAt: started, UpdatedAt: updated,
			GitBranch: gitBranch, GitRemote: gitRemote,
		}},
		Entries: entries, Warnings: warnings,
	}, nil
}

func isCodexInjectedText(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"# AGENTS.md instructions",
		"<environment_context>",
		"<skill>",
		"<user_action>",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}
