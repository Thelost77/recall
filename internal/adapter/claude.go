package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/model"
)

type Claude struct {
	Root string
}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) Discover(ctx context.Context) ([]model.Source, error) {
	return fileSources(ctx, c.Name(), c.Root, func(path string) bool {
		for _, part := range strings.Split(path, string([]byte{filepathSeparator})) {
			if part == "subagents" {
				return true
			}
		}
		return false
	})
}

const filepathSeparator = byte('/')

func (c *Claude) Parse(_ context.Context, source model.Source) (model.ParsedSource, error) {
	var sessionID, cwd, firstUser, gitBranch string
	var started, updated time.Time
	var entries []model.Entry

	err := decodeJSONValues(source.Path, func(raw json.RawMessage) error {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if candidate := stringValue(value, "sessionId"); candidate != "" {
			sessionID = candidate
		}
		if candidate := stringValue(value, "cwd"); candidate != "" {
			cwd = candidate
		}
		if candidate := stringValue(value, "gitBranch"); candidate != "" {
			gitBranch = candidate
		}
		timestamp := parseISO(stringValue(value, "timestamp"))
		updateBounds(&started, &updated, timestamp)

		typeName := stringValue(value, "type")
		if typeName != "user" && typeName != "assistant" {
			return nil
		}
		if isMeta, _ := value["isMeta"].(bool); isMeta {
			return nil
		}
		message, ok := value["message"].(map[string]any)
		if !ok {
			return nil
		}
		role := stringValue(message, "role")
		if role != "user" && role != "assistant" {
			role = typeName
		}
		text := contentText(message["content"], map[string]bool{"text": true})
		if text == "" || isClaudeCommandWrapper(text) {
			return nil
		}
		if role == "user" && firstUser == "" {
			firstUser = text
		}
		entries = append(entries, model.Entry{
			NativeID: stringValue(value, "uuid"),
			ParentID: stringValue(value, "parentUuid"),
			Role:     role, Kind: "message", Timestamp: timestamp, Text: text,
		})
		return nil
	})
	if err != nil {
		return model.ParsedSource{}, fmt.Errorf("parse Claude session %s: %w", source.Path, err)
	}
	if sessionID == "" {
		return model.ParsedSource{}, fmt.Errorf("parse Claude session %s: missing session ID", source.Path)
	}

	sessionKey := stableKey("claude", sessionID)
	for index := range entries {
		entry := &entries[index]
		entry.SessionKey = sessionKey
		entry.Key = stableKey("claude", sessionID, entry.NativeID, entry.Kind)
	}
	return model.ParsedSource{
		Sessions: []model.Session{{
			Key: sessionKey, Harness: "claude", NativeID: sessionID,
			Name: firstLineTitle(firstUser), CWD: cwd,
			SourceKey: source.Key, SourcePath: source.Path,
			StartedAt: started, UpdatedAt: updated, GitBranch: gitBranch,
		}},
		Entries: entries,
	}, nil
}

func isClaudeCommandWrapper(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "<local-command-") ||
		strings.HasPrefix(trimmed, "<command-name>") ||
		strings.HasPrefix(trimmed, "<command-message>")
}
