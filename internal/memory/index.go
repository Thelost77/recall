package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/chunk"
	"github.com/Thelost77/recall/internal/embed"
	_ "modernc.org/sqlite"
)

const memorySchemaVersion = 1
const memoryEmbeddingBatchSize = 32

type Index struct {
	db   *sql.DB
	path string
}

type IndexStats struct {
	Memories          int
	Active            int
	Archived          int
	Embeddings        int
	PendingEmbeddings int
	EmbeddingDims     int
	LastIndexedAt     string
}

type RefreshResult struct {
	Discovered     int
	Updated        int
	Unchanged      int
	Deleted        int
	Embedded       int
	EmbeddingError error
	Warnings       []Warning
}

type PendingMemory struct {
	ID      string
	Content string
}

func OpenIndex(ctx context.Context, path string) (*Index, error) {
	if path == "" {
		return nil, fmt.Errorf("memory index path is empty")
	}
	directory := filepath.Dir(path)
	_, statErr := os.Stat(directory)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return nil, fmt.Errorf("inspect memory index directory %s: %w", directory, statErr)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create memory index directory %s: %w", directory, err)
	}
	if created {
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("protect memory index directory %s: %w", directory, err)
		}
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open memory index %s: %w", path, err)
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
			return nil, fmt.Errorf("configure memory index: %w", err)
		}
	}
	index := &Index{db: db, path: path}
	if err := index.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("protect memory index %s: %w", path+suffix, err)
		}
	}
	return index, nil
}

func (i *Index) Close() error { return i.db.Close() }
func (i *Index) DB() *sql.DB  { return i.db }
func (i *Index) Path() string { return i.path }

func (i *Index) migrate(ctx context.Context) error {
	var version int
	if err := i.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read memory index schema version: %w", err)
	}
	if version > memorySchemaVersion {
		return fmt.Errorf("memory index schema version %d is newer than supported version %d", version, memorySchemaVersion)
	}
	if version == memorySchemaVersion {
		return nil
	}
	if version != 0 {
		return fmt.Errorf("no migration from memory index schema version %d", version)
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE memory (
			id TEXT PRIMARY KEY,
			canonical_path TEXT NOT NULL UNIQUE,
			content_hash TEXT NOT NULL,
			current_revision INTEGER NOT NULL,
			content TEXT NOT NULL,
			scope TEXT NOT NULL,
			project_key TEXT NOT NULL DEFAULT '',
			project_remote TEXT NOT NULL DEFAULT '',
			project_name TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			archived_at INTEGER,
			grams TEXT NOT NULL,
			embedding BLOB,
			embedding_fingerprint TEXT NOT NULL DEFAULT '',
			indexed_at INTEGER NOT NULL
		)`,
		`CREATE INDEX memory_scope_idx ON memory(scope, project_key, archived_at)`,
		`CREATE INDEX memory_embedding_idx ON memory(embedding_fingerprint)`,
		`CREATE VIRTUAL TABLE memory_fts USING fts5(
			content, source, project_name, memory_id,
			tokenize = 'unicode61 remove_diacritics 2'
		)`,
		`CREATE VIRTUAL TABLE memory_grams USING fts5(grams, tokenize = 'unicode61')`,
		`CREATE TRIGGER memory_insert AFTER INSERT ON memory BEGIN
			INSERT INTO memory_fts(rowid, content, source, project_name, memory_id)
			VALUES (NEW.rowid, NEW.content, NEW.source, NEW.project_name, NEW.id);
			INSERT INTO memory_grams(rowid, grams) VALUES (NEW.rowid, NEW.grams);
		END`,
		`CREATE TRIGGER memory_delete AFTER DELETE ON memory BEGIN
			DELETE FROM memory_fts WHERE rowid = OLD.rowid;
			DELETE FROM memory_grams WHERE rowid = OLD.rowid;
		END`,
		`CREATE TRIGGER memory_update AFTER UPDATE OF content, source, project_name, id, grams ON memory BEGIN
			DELETE FROM memory_fts WHERE rowid = OLD.rowid;
			DELETE FROM memory_grams WHERE rowid = OLD.rowid;
			INSERT INTO memory_fts(rowid, content, source, project_name, memory_id)
			VALUES (NEW.rowid, NEW.content, NEW.source, NEW.project_name, NEW.id);
			INSERT INTO memory_grams(rowid, grams) VALUES (NEW.rowid, NEW.grams);
		END`,
		`PRAGMA user_version = 1`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create memory index schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory index schema: %w", err)
	}
	return nil
}

func EnsureIndexOutsideCanonical(memoryDirectory, indexPath string) error {
	memoryAbs, err := filepath.Abs(memoryDirectory)
	if err != nil {
		return err
	}
	indexAbs, err := filepath.Abs(indexPath)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Clean(memoryAbs), filepath.Clean(indexAbs))
	if err != nil {
		return err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return fmt.Errorf("memory index must not be stored in canonical memory directory %s", memoryDirectory)
	}
	return nil
}

func Refresh(ctx context.Context, files FileStore, index *Index, provider embed.Provider) (RefreshResult, error) {
	scan, err := files.Scan()
	if err != nil {
		return RefreshResult{}, err
	}
	result := RefreshResult{Discovered: len(scan.Records), Warnings: scan.Warnings}
	known, err := index.knownFiles(ctx)
	if err != nil {
		return result, err
	}
	present := map[string]bool{}
	for _, item := range scan.Records {
		present[item.Path] = true
		hash := sha256.Sum256(item.Raw)
		hashText := hex.EncodeToString(hash[:])
		if known[item.Path] == hashText {
			result.Unchanged++
			continue
		}
		if err := index.upsert(ctx, item.Path, hashText, item.Record); err != nil {
			return result, fmt.Errorf("index memory %s: %w", item.Record.ID, err)
		}
		result.Updated++
	}
	for _, warning := range scan.Warnings {
		if warning.Type == "invalid_record" {
			present[warning.Path] = true
		}
	}
	result.Deleted, err = index.deleteMissing(ctx, present)
	if err != nil {
		return result, err
	}
	if provider != nil {
		result.Embedded, result.EmbeddingError = index.Backfill(ctx, provider)
	}
	if err := index.setMeta(ctx, "last_indexed_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return result, err
	}
	return result, nil
}

func (i *Index) knownFiles(ctx context.Context) (map[string]string, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT canonical_path, content_hash FROM memory`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return nil, err
		}
		result[path] = hash
	}
	return result, rows.Err()
}

func (i *Index) upsert(ctx context.Context, path, hash string, record Record) error {
	var archived any
	if record.ArchivedAt != nil {
		archived = record.ArchivedAt.UnixMilli()
	}
	_, err := i.db.ExecContext(ctx, `
		INSERT INTO memory(
			id, canonical_path, content_hash, current_revision, content, scope,
			project_key, project_remote, project_name, source, created_at, updated_at,
			archived_at, grams, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			canonical_path=excluded.canonical_path, content_hash=excluded.content_hash,
			current_revision=excluded.current_revision, scope=excluded.scope,
			project_key=excluded.project_key, project_remote=excluded.project_remote,
			project_name=excluded.project_name, source=excluded.source,
			created_at=excluded.created_at, updated_at=excluded.updated_at,
			archived_at=excluded.archived_at, grams=excluded.grams,
			embedding=CASE WHEN memory.content=excluded.content THEN memory.embedding ELSE NULL END,
			embedding_fingerprint=CASE WHEN memory.content=excluded.content THEN memory.embedding_fingerprint ELSE '' END,
			content=excluded.content, indexed_at=excluded.indexed_at`,
		record.ID, path, hash, record.CurrentRevision, record.CurrentContent(), record.Scope,
		record.ProjectKey, record.ProjectRemote, record.ProjectName, record.Source,
		record.CreatedAt.UnixMilli(), record.UpdatedAt.UnixMilli(), archived,
		chunk.Grams(record.CurrentContent()), time.Now().UnixMilli())
	return err
}

func (i *Index) deleteMissing(ctx context.Context, present map[string]bool) (int, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT canonical_path FROM memory`)
	if err != nil {
		return 0, err
	}
	var missing []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return 0, err
		}
		if !present[path] {
			missing = append(missing, path)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, path := range missing {
		if _, err := i.db.ExecContext(ctx, `DELETE FROM memory WHERE canonical_path = ?`, path); err != nil {
			return 0, err
		}
	}
	return len(missing), nil
}

func (i *Index) Pending(ctx context.Context, fingerprint string, limit int) ([]PendingMemory, error) {
	rows, err := i.db.QueryContext(ctx, `
		SELECT id, content FROM memory
		WHERE embedding IS NULL OR embedding_fingerprint <> ?
		ORDER BY id LIMIT ?`, fingerprint, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PendingMemory
	for rows.Next() {
		var item PendingMemory
		if err := rows.Scan(&item.ID, &item.Content); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (i *Index) Backfill(ctx context.Context, provider embed.Provider) (int, error) {
	fingerprint := provider.Fingerprint()
	total := 0
	for {
		pending, err := i.Pending(ctx, fingerprint, memoryEmbeddingBatchSize)
		if err != nil {
			return total, err
		}
		if len(pending) == 0 {
			return total, nil
		}
		texts := make([]string, len(pending))
		for position, item := range pending {
			texts[position] = item.Content
		}
		vectors, err := provider.Embed(ctx, texts)
		if err != nil {
			return total, err
		}
		if len(vectors) != len(pending) || len(vectors) == 0 || len(vectors[0]) == 0 {
			return total, fmt.Errorf("embedder returned an invalid memory vector batch")
		}
		dimensions := len(vectors[0])
		tx, err := i.db.BeginTx(ctx, nil)
		if err != nil {
			return total, err
		}
		for position, vector := range vectors {
			if len(vector) != dimensions {
				_ = tx.Rollback()
				return total, fmt.Errorf("memory embedding batch contains inconsistent dimensions")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memory SET embedding = ?, embedding_fingerprint = ? WHERE id = ?`, embed.Encode(vector), fingerprint, pending[position].ID); err != nil {
				_ = tx.Rollback()
				return total, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('embedding_dimensions', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(dimensions)); err != nil {
			_ = tx.Rollback()
			return total, err
		}
		if err := tx.Commit(); err != nil {
			return total, err
		}
		total += len(pending)
	}
}

func (i *Index) ClearEmbeddings(ctx context.Context) error {
	_, err := i.db.ExecContext(ctx, `UPDATE memory SET embedding = NULL, embedding_fingerprint = ''`)
	return err
}

func (i *Index) Stats(ctx context.Context, fingerprint string) (IndexStats, error) {
	var result IndexStats
	queries := []struct {
		query string
		dest  *int
		arg   bool
	}{
		{`SELECT count(*) FROM memory`, &result.Memories, false},
		{`SELECT count(*) FROM memory WHERE archived_at IS NULL`, &result.Active, false},
		{`SELECT count(*) FROM memory WHERE archived_at IS NOT NULL`, &result.Archived, false},
		{`SELECT count(*) FROM memory WHERE embedding IS NOT NULL AND embedding_fingerprint = ?`, &result.Embeddings, true},
		{`SELECT count(*) FROM memory WHERE embedding IS NULL OR embedding_fingerprint <> ?`, &result.PendingEmbeddings, true},
	}
	for _, item := range queries {
		var err error
		if item.arg {
			err = i.db.QueryRowContext(ctx, item.query, fingerprint).Scan(item.dest)
		} else {
			err = i.db.QueryRowContext(ctx, item.query).Scan(item.dest)
		}
		if err != nil {
			return IndexStats{}, err
		}
	}
	result.LastIndexedAt, _ = i.meta(ctx, "last_indexed_at")
	dimensions, _ := i.meta(ctx, "embedding_dimensions")
	result.EmbeddingDims, _ = strconv.Atoi(dimensions)
	return result, nil
}

func (i *Index) setMeta(ctx context.Context, key, value string) error {
	_, err := i.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (i *Index) meta(ctx context.Context, key string) (string, error) {
	var value string
	err := i.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}
