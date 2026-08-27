package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/model"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

type Store struct {
	db   *sql.DB
	path string
}

type Stats struct {
	Sources           int
	Sessions          int
	Chunks            int
	Embeddings        int
	PendingEmbeddings int
	ParserErrors      int
	LastIndexedAt     string
	EmbeddingModel    string
	EmbeddingDims     int
}

type PendingChunk struct {
	ID   int64
	Text string
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("index path is empty")
	}
	directory := filepath.Dir(path)
	_, statErr := os.Stat(directory)
	createdDirectory := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !createdDirectory {
		return nil, fmt.Errorf("inspect index directory %s: %w", directory, statErr)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create index directory %s: %w", directory, err)
	}
	if createdDirectory {
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("protect index directory %s: %w", directory, err)
		}
	}

	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open index %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA synchronous = NORMAL`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure index: %w", err)
		}
	}
	store := &Store{db: db, path: path}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("protect index %s: %w", path, err)
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read index schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("index schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	if version != 0 {
		return fmt.Errorf("no migration from index schema version %d", version)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE source (
			key TEXT PRIMARY KEY,
			harness TEXT NOT NULL,
			path TEXT NOT NULL,
			version TEXT NOT NULL,
			indexed_at INTEGER NOT NULL,
			last_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX source_harness_idx ON source(harness)`,
		`CREATE TABLE session (
			key TEXT PRIMARY KEY,
			harness TEXT NOT NULL,
			native_id TEXT NOT NULL,
			parent_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			cwd TEXT NOT NULL DEFAULT '',
			source_key TEXT NOT NULL REFERENCES source(key) ON DELETE CASCADE,
			source_path TEXT NOT NULL,
			started_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			git_branch TEXT NOT NULL DEFAULT '',
			git_remote TEXT NOT NULL DEFAULT '',
			generation TEXT NOT NULL,
			UNIQUE(harness, native_id)
		)`,
		`CREATE INDEX session_source_idx ON session(source_key)`,
		`CREATE INDEX session_cwd_idx ON session(cwd)`,
		`CREATE TABLE chunk (
			id INTEGER PRIMARY KEY,
			chunk_key TEXT NOT NULL UNIQUE,
			session_key TEXT NOT NULL REFERENCES session(key) ON DELETE CASCADE,
			entry_key TEXT NOT NULL,
			entry_native_id TEXT NOT NULL DEFAULT '',
			entry_parent_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			part INTEGER NOT NULL,
			role TEXT NOT NULL,
			timestamp INTEGER NOT NULL DEFAULT 0,
			text TEXT NOT NULL,
			text_hash TEXT NOT NULL,
			grams TEXT NOT NULL,
			embedding BLOB,
			embedding_fingerprint TEXT NOT NULL DEFAULT '',
			generation TEXT NOT NULL
		)`,
		`CREATE INDEX chunk_session_idx ON chunk(session_key)`,
		`CREATE INDEX chunk_embedding_idx ON chunk(embedding_fingerprint)`,
		`CREATE VIRTUAL TABLE chunk_fts USING fts5(
			content, session_name, cwd, native_id, git_branch,
			tokenize = 'unicode61 remove_diacritics 2'
		)`,
		`CREATE VIRTUAL TABLE chunk_grams USING fts5(grams, tokenize = 'unicode61')`,
		`CREATE TABLE parser_error (
			source_key TEXT PRIMARY KEY,
			harness TEXT NOT NULL,
			path TEXT NOT NULL,
			error TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TRIGGER chunk_insert AFTER INSERT ON chunk BEGIN
			INSERT INTO chunk_fts(rowid, content, session_name, cwd, native_id, git_branch)
			SELECT NEW.id, NEW.text, s.name, s.cwd, s.native_id, s.git_branch
			FROM session AS s WHERE s.key = NEW.session_key;
			INSERT INTO chunk_grams(rowid, grams) VALUES (NEW.id, NEW.grams);
		END`,
		`CREATE TRIGGER chunk_delete AFTER DELETE ON chunk BEGIN
			DELETE FROM chunk_fts WHERE rowid = OLD.id;
			DELETE FROM chunk_grams WHERE rowid = OLD.id;
		END`,
		`CREATE TRIGGER chunk_update AFTER UPDATE OF session_key, text, grams ON chunk BEGIN
			DELETE FROM chunk_fts WHERE rowid = OLD.id;
			DELETE FROM chunk_grams WHERE rowid = OLD.id;
			INSERT INTO chunk_fts(rowid, content, session_name, cwd, native_id, git_branch)
			SELECT NEW.id, NEW.text, s.name, s.cwd, s.native_id, s.git_branch
			FROM session AS s WHERE s.key = NEW.session_key;
			INSERT INTO chunk_grams(rowid, grams) VALUES (NEW.id, NEW.grams);
		END`,
		`PRAGMA user_version = 1`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create index schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit index schema: %w", err)
	}
	return nil
}

func (s *Store) SourceVersions(ctx context.Context, harness string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, version FROM source WHERE harness = ?`, harness)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var key, version string
		if err := rows.Scan(&key, &version); err != nil {
			return nil, err
		}
		result[key] = version
	}
	return result, rows.Err()
}

func (s *Store) UpsertSource(ctx context.Context, source model.Source, parsed model.ParsedSource, chunks []model.Chunk) error {
	generation := strconv.FormatInt(time.Now().UnixNano(), 10)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source(key, harness, path, version, indexed_at, last_error)
		VALUES (?, ?, ?, ?, ?, '')
		ON CONFLICT(key) DO UPDATE SET
			harness=excluded.harness, path=excluded.path, version=excluded.version,
			indexed_at=excluded.indexed_at, last_error=''`,
		source.Key, source.Harness, source.Path, source.Version, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("upsert source %s: %w", source.Path, err)
	}
	for _, session := range parsed.Sessions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session(
				key, harness, native_id, parent_id, name, cwd, source_key, source_path,
				started_at, updated_at, git_branch, git_remote, generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				harness=excluded.harness, native_id=excluded.native_id, parent_id=excluded.parent_id,
				name=excluded.name, cwd=excluded.cwd, source_key=excluded.source_key,
				source_path=excluded.source_path, started_at=excluded.started_at,
				updated_at=excluded.updated_at, git_branch=excluded.git_branch,
				git_remote=excluded.git_remote, generation=excluded.generation`,
			session.Key, session.Harness, session.NativeID, session.ParentID, session.Name,
			session.CWD, source.Key, session.SourcePath, millis(session.StartedAt),
			millis(session.UpdatedAt), session.GitBranch, session.GitRemote, generation); err != nil {
			return fmt.Errorf("upsert session %s: %w", session.NativeID, err)
		}
	}
	for _, item := range chunks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chunk(
				chunk_key, session_key, entry_key, entry_native_id, entry_parent_id, kind,
				part, role, timestamp, text, text_hash, grams, generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(chunk_key) DO UPDATE SET
				session_key=excluded.session_key, entry_key=excluded.entry_key,
				entry_native_id=excluded.entry_native_id, entry_parent_id=excluded.entry_parent_id,
				kind=excluded.kind, part=excluded.part, role=excluded.role,
				timestamp=excluded.timestamp, text=excluded.text, grams=excluded.grams,
				embedding=CASE WHEN chunk.text_hash=excluded.text_hash THEN chunk.embedding ELSE NULL END,
				embedding_fingerprint=CASE WHEN chunk.text_hash=excluded.text_hash THEN chunk.embedding_fingerprint ELSE '' END,
				text_hash=excluded.text_hash, generation=excluded.generation`,
			item.Key, item.SessionKey, item.EntryKey, item.EntryNativeID, item.EntryParentID,
			item.Kind, item.Part, item.Role, millis(item.Timestamp), item.Text, item.TextHash,
			item.Grams, generation); err != nil {
			return fmt.Errorf("upsert chunk %s: %w", item.Key, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM chunk
		WHERE session_key IN (SELECT key FROM session WHERE source_key = ?)
		  AND generation <> ?`, source.Key, generation); err != nil {
		return fmt.Errorf("delete stale chunks for %s: %w", source.Path, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session WHERE source_key = ? AND generation <> ?`, source.Key, generation); err != nil {
		return fmt.Errorf("delete stale sessions for %s: %w", source.Path, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM parser_error WHERE source_key = ?`, source.Key); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordSourceError(ctx context.Context, source model.Source, parseErr error) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO parser_error(source_key, harness, path, error, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_key) DO UPDATE SET
			harness=excluded.harness, path=excluded.path, error=excluded.error,
			created_at=excluded.created_at`, source.Key, source.Harness, source.Path,
		parseErr.Error(), time.Now().UnixMilli())
	return err
}

func (s *Store) DeleteMissingSources(ctx context.Context, harness string, present map[string]bool) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM source WHERE harness = ?`, harness)
	if err != nil {
		return 0, err
	}
	var missing []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, err
		}
		if !present[key] {
			missing = append(missing, key)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, key := range missing {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM source WHERE key = ?`, key); err != nil {
			return 0, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM parser_error WHERE source_key = ?`, key); err != nil {
			return 0, err
		}
	}
	return len(missing), nil
}

func (s *Store) PendingChunks(ctx context.Context, fingerprint string, limit int) ([]PendingChunk, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, text FROM chunk
		WHERE embedding IS NULL OR embedding_fingerprint <> ?
		ORDER BY id LIMIT ?`, fingerprint, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PendingChunk
	for rows.Next() {
		var item PendingChunk
		if err := rows.Scan(&item.ID, &item.Text); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) UpdateEmbeddings(ctx context.Context, fingerprint string, dimensions int, ids []int64, vectors [][]byte) error {
	if len(ids) != len(vectors) {
		return fmt.Errorf("embedding IDs and vectors have different lengths")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for index := range ids {
		if _, err := tx.ExecContext(ctx, `
			UPDATE chunk SET embedding = ?, embedding_fingerprint = ? WHERE id = ?`,
			vectors[index], fingerprint, ids[index]); err != nil {
			return err
		}
	}
	if err := setMetaTx(ctx, tx, "embedding_fingerprint", fingerprint); err != nil {
		return err
	}
	if err := setMetaTx(ctx, tx, "embedding_dimensions", strconv.Itoa(dimensions)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClearEmbeddings(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chunk SET embedding = NULL, embedding_fingerprint = ''`)
	return err
}

func (s *Store) MarkIndexed(ctx context.Context) error {
	return s.SetMeta(ctx, "last_indexed_at", time.Now().UTC().Format(time.RFC3339))
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) Stats(ctx context.Context, fingerprint string) (Stats, error) {
	var result Stats
	queries := []struct {
		query string
		dest  *int
	}{
		{`SELECT count(*) FROM source`, &result.Sources},
		{`SELECT count(*) FROM session`, &result.Sessions},
		{`SELECT count(*) FROM chunk`, &result.Chunks},
		{`SELECT count(*) FROM chunk WHERE embedding IS NOT NULL AND embedding_fingerprint = ?`, &result.Embeddings},
		{`SELECT count(*) FROM chunk WHERE embedding IS NULL OR embedding_fingerprint <> ?`, &result.PendingEmbeddings},
		{`SELECT count(*) FROM parser_error`, &result.ParserErrors},
	}
	for index, item := range queries {
		var err error
		if index == 3 || index == 4 {
			err = s.db.QueryRowContext(ctx, item.query, fingerprint).Scan(item.dest)
		} else {
			err = s.db.QueryRowContext(ctx, item.query).Scan(item.dest)
		}
		if err != nil {
			return Stats{}, err
		}
	}
	result.LastIndexedAt, _ = s.Meta(ctx, "last_indexed_at")
	result.EmbeddingModel = fingerprint
	dimensions, _ := s.Meta(ctx, "embedding_dimensions")
	result.EmbeddingDims, _ = strconv.Atoi(dimensions)
	return result, nil
}

func (s *Store) ParserErrors(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT harness, path, error FROM parser_error ORDER BY harness, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var harness, path, message string
		if err := rows.Scan(&harness, &path, &message); err != nil {
			return nil, err
		}
		values = append(values, fmt.Sprintf("%s: %s: %s", harness, path, message))
	}
	sort.Strings(values)
	return values, rows.Err()
}

func setMetaTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func millis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

func Placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}
