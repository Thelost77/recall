package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/model"
)

type Pi struct {
	Root string
}

func (p *Pi) Name() string { return "pi" }

func (p *Pi) Discover(ctx context.Context) ([]model.Source, error) {
	return fileSources(ctx, p.Name(), p.Root, nil)
}

func (p *Pi) Parse(_ context.Context, source model.Source) (model.ParsedSource, error) {
	var sessionID, cwd, name, firstUser string
	var started, updated time.Time
	var entries []model.Entry

	err := decodeJSONValues(source.Path, func(raw json.RawMessage) error {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		timestamp := parseISO(stringValue(value, "timestamp"))
		updateBounds(&started, &updated, timestamp)

		switch stringValue(value, "type") {
		case "session":
			sessionID = stringValue(value, "id")
			cwd = stringValue(value, "cwd")
		case "session_info":
			if candidate := stringValue(value, "name"); candidate != "" {
				name = candidate
			}
		case "compaction", "branch_summary":
			text := stringValue(value, "summary")
			if strings.TrimSpace(text) == "" {
				return nil
			}
			nativeID := stringValue(value, "id")
			entries = append(entries, model.Entry{
				NativeID: nativeID,
				ParentID: stringValue(value, "parentId"),
				Role:     "assistant", Kind: stringValue(value, "type"),
				Timestamp: timestamp, Text: text,
			})
		case "message":
			message, ok := value["message"].(map[string]any)
			if !ok {
				return nil
			}
			role := stringValue(message, "role")
			if role != "user" && role != "assistant" {
				return nil
			}
			text := contentText(message["content"], map[string]bool{"text": true})
			if text == "" {
				return nil
			}
			if role == "user" && firstUser == "" {
				firstUser = text
			}
			entries = append(entries, model.Entry{
				NativeID: stringValue(value, "id"),
				ParentID: stringValue(value, "parentId"),
				Role:     role, Kind: "message", Timestamp: timestamp, Text: text,
			})
		}
		return nil
	})
	if err != nil {
		return model.ParsedSource{}, fmt.Errorf("parse Pi session %s: %w", source.Path, err)
	}
	if sessionID == "" {
		return model.ParsedSource{}, fmt.Errorf("parse Pi session %s: missing session header", source.Path)
	}
	if name == "" {
		name = firstLineTitle(firstUser)
	}
	sessionKey := stableKey("pi", sessionID)
	for index := range entries {
		entry := &entries[index]
		entry.SessionKey = sessionKey
		entry.Key = stableKey("pi", sessionID, entry.NativeID, entry.Kind)
	}

	return model.ParsedSource{
		Sessions: []model.Session{{
			Key: sessionKey, Harness: "pi", NativeID: sessionID, Name: name,
			CWD: cwd, SourceKey: source.Key, SourcePath: source.Path,
			StartedAt: started, UpdatedAt: updated,
		}},
		Entries: entries,
	}, nil
}
