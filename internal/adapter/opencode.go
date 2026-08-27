package adapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/Thelost77/recall/internal/model"

	_ "modernc.org/sqlite"
)

type OpenCode struct {
	DBPath string
	db     *sql.DB
}

func (o *OpenCode) Name() string { return "opencode" }

func (o *OpenCode) Discover(ctx context.Context) ([]model.Source, error) {
	if err := o.open(ctx); err != nil {
		return nil, err
	}
	if err := requireColumns(ctx, o.db, "session", []string{
		"id", "parent_id", "directory", "title", "time_created", "time_updated",
	}); err != nil {
		return nil, fmt.Errorf("unsupported OpenCode schema: %w", err)
	}
	if err := requireColumns(ctx, o.db, "message", []string{"id", "session_id", "time_created", "data"}); err != nil {
		return nil, fmt.Errorf("unsupported OpenCode schema: %w", err)
	}
	if err := requireColumns(ctx, o.db, "part", []string{"id", "message_id", "session_id", "time_created", "data"}); err != nil {
		return nil, fmt.Errorf("unsupported OpenCode schema: %w", err)
	}

	rows, err := o.db.QueryContext(ctx, `
		SELECT id, COALESCE(parent_id, ''), directory, title,
		       time_created, time_updated, COALESCE(agent, ''), COALESCE(model, '')
		FROM session
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list OpenCode sessions: %w", err)
	}
	defer rows.Close()

	var sources []model.Source
	for rows.Next() {
		var id, parentID, directory, title, agent, modelName string
		var created, updated int64
		if err := rows.Scan(&id, &parentID, &directory, &title, &created, &updated, &agent, &modelName); err != nil {
			return nil, fmt.Errorf("read OpenCode session metadata: %w", err)
		}
		sources = append(sources, model.Source{
			Key: stableKey("opencode", id), Harness: "opencode", Path: o.DBPath,
			Version: "opencode-v1:" + strconv.FormatInt(updated, 10), NativeID: id,
			Metadata: map[string]string{
				"parent_id": parentID, "directory": directory, "title": title,
				"created": strconv.FormatInt(created, 10), "updated": strconv.FormatInt(updated, 10),
				"agent": agent, "model": modelName,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list OpenCode sessions: %w", err)
	}
	return sources, nil
}

func (o *OpenCode) Parse(ctx context.Context, source model.Source) (model.ParsedSource, error) {
	if err := o.open(ctx); err != nil {
		return model.ParsedSource{}, err
	}
	if source.NativeID == "" {
		return model.ParsedSource{}, fmt.Errorf("parse OpenCode source: missing session ID")
	}

	rows, err := o.db.QueryContext(ctx, `
		SELECT m.id, m.time_created, m.data, p.id, p.time_created, p.data
		FROM message AS m
		JOIN part AS p ON p.message_id = m.id
		WHERE m.session_id = ?
		ORDER BY m.time_created, m.id, p.time_created, p.id`, source.NativeID)
	if err != nil {
		return model.ParsedSource{}, fmt.Errorf("read OpenCode session %s: %w", source.NativeID, err)
	}
	defer rows.Close()

	sessionKey := stableKey("opencode", source.NativeID)
	var entries []model.Entry
	for rows.Next() {
		var messageID, messageData, partID, partData string
		var messageCreated, partCreated int64
		if err := rows.Scan(&messageID, &messageCreated, &messageData, &partID, &partCreated, &partData); err != nil {
			return model.ParsedSource{}, fmt.Errorf("read OpenCode session %s: %w", source.NativeID, err)
		}
		var messageValue, partValue map[string]any
		if err := json.Unmarshal([]byte(messageData), &messageValue); err != nil {
			return model.ParsedSource{}, fmt.Errorf("decode OpenCode message %s: %w", messageID, err)
		}
		if err := json.Unmarshal([]byte(partData), &partValue); err != nil {
			return model.ParsedSource{}, fmt.Errorf("decode OpenCode part %s: %w", partID, err)
		}
		if stringValue(partValue, "type") != "text" {
			continue
		}
		if synthetic, _ := partValue["synthetic"].(bool); synthetic {
			continue
		}
		role := stringValue(messageValue, "role")
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(stringValue(partValue, "text"))
		if text == "" {
			continue
		}
		entries = append(entries, model.Entry{
			Key: stableKey("opencode", source.NativeID, partID), SessionKey: sessionKey,
			NativeID: partID, ParentID: stringValue(messageValue, "parentID"),
			Role: role, Kind: "message", Timestamp: unixMillis(partCreated), Text: text,
		})
	}
	if err := rows.Err(); err != nil {
		return model.ParsedSource{}, fmt.Errorf("read OpenCode session %s: %w", source.NativeID, err)
	}

	created, _ := strconv.ParseInt(source.Metadata["created"], 10, 64)
	updated, _ := strconv.ParseInt(source.Metadata["updated"], 10, 64)
	return model.ParsedSource{
		Sessions: []model.Session{{
			Key: sessionKey, Harness: "opencode", NativeID: source.NativeID,
			ParentID: source.Metadata["parent_id"], Name: source.Metadata["title"],
			CWD: source.Metadata["directory"], SourceKey: source.Key, SourcePath: source.Path,
			StartedAt: unixMillis(created), UpdatedAt: unixMillis(updated),
		}},
		Entries: entries,
	}, nil
}

func (o *OpenCode) Close() error {
	if o.db == nil {
		return nil
	}
	return o.db.Close()
}

func (o *OpenCode) open(ctx context.Context) error {
	if o.db != nil {
		return nil
	}
	if o.DBPath == "" {
		output, err := exec.CommandContext(ctx, "opencode", "db", "path").Output()
		if err != nil {
			return fmt.Errorf("locate OpenCode database with `opencode db path`: %w", err)
		}
		o.DBPath = strings.TrimSpace(string(output))
	}
	if o.DBPath == "" {
		return fmt.Errorf("locate OpenCode database: command returned an empty path")
	}

	dsn := (&url.URL{Scheme: "file", Path: o.DBPath, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open OpenCode database %s: %w", o.DBPath, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		db.Close()
		return fmt.Errorf("open OpenCode database read-only: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return fmt.Errorf("configure OpenCode database timeout: %w", err)
	}
	o.db = db
	return nil
}

func requireColumns(ctx context.Context, db *sql.DB, table string, required []string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		found[name] = true
	}
	var missing []string
	for _, name := range required {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("table %s is missing columns: %s", table, strings.Join(missing, ", "))
	}
	return rows.Err()
}
