package memory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type FileStore struct {
	Directory string
	Now       func() time.Time
	Random    io.Reader
}

type ScanResult struct {
	Records  []ScannedRecord
	Warnings []Warning
}

type ScannedRecord struct {
	Path   string
	Record Record
	Raw    []byte
}

var conflictName = regexp.MustCompile(`(?i)^(.+)\.sync-conflict-\d{8}-\d{6}-[a-z0-9]+(\.[^.]+)$`)

func (s FileStore) RecordsDirectory() string { return filepath.Join(s.Directory, "records") }

func (s FileStore) Scan() (ScanResult, error) {
	directory := s.RecordsDirectory()
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return ScanResult{Records: []ScannedRecord{}, Warnings: []Warning{}}, nil
	}
	if err != nil {
		return ScanResult{}, fmt.Errorf("scan memory records %s: %w", directory, err)
	}
	result := ScanResult{Records: []ScannedRecord{}, Warnings: []Warning{}}
	conflicts := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(directory, name)
		if match := conflictName.FindStringSubmatch(name); match != nil {
			canonical := filepath.Join(directory, match[1]+match[2])
			conflicts[canonical] = append(conflicts[canonical], path)
			continue
		}
		if strings.ToLower(filepath.Ext(name)) != ".json" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			result.Warnings = append(result.Warnings, invalidWarning(path, "read record: "+err.Error()))
			continue
		}
		record, err := parseRecord(path, raw)
		if err != nil {
			result.Warnings = append(result.Warnings, invalidWarning(path, err.Error()))
			continue
		}
		result.Records = append(result.Records, ScannedRecord{Path: path, Record: record, Raw: raw})
	}
	canonicalPaths := make([]string, 0, len(conflicts))
	for canonical := range conflicts {
		canonicalPaths = append(canonicalPaths, canonical)
	}
	sort.Strings(canonicalPaths)
	for _, canonical := range canonicalPaths {
		paths := conflicts[canonical]
		sort.Strings(paths)
		id := strings.TrimSuffix(filepath.Base(canonical), filepath.Ext(canonical))
		if !idPattern.MatchString(id) {
			id = ""
		}
		result.Warnings = append(result.Warnings, Warning{
			Type: "syncthing_conflict", Message: "Syncthing conflict requires manual resolution",
			Path: canonical, MemoryID: id, ConflictPaths: paths,
		})
	}
	sort.Slice(result.Records, func(i, j int) bool { return result.Records[i].Record.ID < result.Records[j].Record.ID })
	return result, nil
}

func (s FileStore) Create(scope, content, source, projectKey, projectRemote, projectName string) (Record, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Record{}, fmt.Errorf("memory content is empty")
	}
	if err := s.ensureDirectories(); err != nil {
		return Record{}, err
	}
	now := s.now()
	for attempts := 0; attempts < 10; attempts++ {
		id, err := s.newID()
		if err != nil {
			return Record{}, err
		}
		record := Record{
			SchemaVersion: SchemaVersion, ID: id, Scope: scope, ProjectKey: projectKey,
			ProjectRemote: projectRemote, ProjectName: projectName, Source: source,
			CreatedAt: now, UpdatedAt: now, CurrentRevision: 1,
			Revisions: []Revision{{Revision: 1, Content: content, CreatedAt: now}},
		}
		path := s.recordPath(id)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return Record{}, fmt.Errorf("inspect memory target %s: %w", path, err)
		}
		if err := s.write(record); err != nil {
			return Record{}, err
		}
		return record, nil
	}
	return Record{}, fmt.Errorf("could not generate a unique memory ID")
}

func (s FileStore) Load(prefix string) (Record, string, error) {
	id, path, err := s.ResolvePrefix(prefix)
	if err != nil {
		return Record{}, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Record{}, "", fmt.Errorf("read memory %s: %w", id, err)
	}
	record, err := parseRecord(path, raw)
	if err != nil {
		return Record{}, "", err
	}
	return record, path, nil
}

func (s FileStore) ResolvePrefix(prefix string) (string, string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", "", fmt.Errorf("memory ID is empty")
	}
	entries, err := os.ReadDir(s.RecordsDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("memory %q not found", prefix)
	}
	if err != nil {
		return "", "", fmt.Errorf("scan memory IDs: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || conflictName.MatchString(name) || filepath.Ext(name) != ".json" {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if idPattern.MatchString(id) && strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", "", fmt.Errorf("memory %q not found", prefix)
	}
	if len(ids) > 1 {
		return "", "", fmt.Errorf("memory ID prefix %q is ambiguous: %s", prefix, strings.Join(ids, ", "))
	}
	return ids[0], s.recordPath(ids[0]), nil
}

func (s FileStore) Edit(prefix, content string) (Record, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Record{}, fmt.Errorf("memory content is empty")
	}
	return s.mutate(prefix, func(record *Record, now time.Time) error {
		next := len(record.Revisions) + 1
		record.Revisions = append(record.Revisions, Revision{Revision: next, Content: content, CreatedAt: now})
		record.CurrentRevision = next
		record.UpdatedAt = now
		return nil
	})
}

func (s FileStore) Forget(prefix string) (Record, error) {
	return s.mutate(prefix, func(record *Record, now time.Time) error {
		if record.ArchivedAt != nil {
			return fmt.Errorf("memory %s is already archived", record.ID)
		}
		record.ArchivedAt = &now
		record.UpdatedAt = now
		return nil
	})
}

func (s FileStore) Restore(prefix string) (Record, error) {
	return s.mutate(prefix, func(record *Record, now time.Time) error {
		if record.ArchivedAt == nil {
			return fmt.Errorf("memory %s is not archived", record.ID)
		}
		record.ArchivedAt = nil
		record.UpdatedAt = now
		return nil
	})
}

func (s FileStore) mutate(prefix string, change func(*Record, time.Time) error) (Record, error) {
	record, path, err := s.Load(prefix)
	if err != nil {
		return Record{}, err
	}
	if conflicts, err := s.conflictsFor(path); err != nil {
		return Record{}, err
	} else if len(conflicts) > 0 {
		return Record{}, fmt.Errorf("memory %s has Syncthing conflicts; resolve manually before changing it: %s", record.ID, strings.Join(conflicts, ", "))
	}
	if err := change(&record, s.now()); err != nil {
		return Record{}, err
	}
	if err := s.write(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s FileStore) conflictsFor(canonical string) ([]string, error) {
	scan, err := s.Scan()
	if err != nil {
		return nil, err
	}
	for _, warning := range scan.Warnings {
		if warning.Type == "syncthing_conflict" && filepath.Clean(warning.Path) == filepath.Clean(canonical) {
			return warning.ConflictPaths, nil
		}
	}
	return nil, nil
}

func (s FileStore) write(record Record) error {
	if err := record.Validate(); err != nil {
		return fmt.Errorf("validate memory %s: %w", record.ID, err)
	}
	if err := s.ensureDirectories(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize memory %s: %w", record.ID, err)
	}
	payload = append(payload, '\n')
	path := s.recordPath(record.ID)
	temporary, err := os.CreateTemp(s.RecordsDirectory(), ".recall-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary memory file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	closeNeeded := true
	defer func() {
		if closeNeeded {
			_ = temporary.Close()
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect temporary memory file: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		return fmt.Errorf("write temporary memory file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary memory file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary memory file: %w", err)
	}
	closeNeeded = false
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace memory %s: %w", record.ID, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect memory %s: %w", record.ID, err)
	}
	if directory, err := os.Open(s.RecordsDirectory()); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (s FileStore) ensureDirectories() error {
	for _, directory := range []string{s.Directory, s.RecordsDirectory()} {
		_, err := os.Stat(directory)
		created := errors.Is(err, os.ErrNotExist)
		if err != nil && !created {
			return fmt.Errorf("inspect memory directory %s: %w", directory, err)
		}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create memory directory %s: %w", directory, err)
		}
		if created {
			if err := os.Chmod(directory, 0o700); err != nil {
				return fmt.Errorf("protect memory directory %s: %w", directory, err)
			}
		}
	}
	return nil
}

func (s FileStore) newID() (string, error) {
	reader := s.Random
	if reader == nil {
		reader = rand.Reader
	}
	buffer := make([]byte, 8)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return "", fmt.Errorf("generate memory ID: %w", err)
	}
	return "m_" + hex.EncodeToString(buffer), nil
}

func (s FileStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s FileStore) recordPath(id string) string {
	return filepath.Join(s.RecordsDirectory(), id+".json")
}

func parseRecord(path string, raw []byte) (Record, error) {
	var record Record
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Record{}, fmt.Errorf("parse %s: trailing JSON data", path)
	}
	if err := record.Validate(); err != nil {
		return Record{}, fmt.Errorf("validate %s: %w", path, err)
	}
	filenameID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if filenameID != record.ID {
		return Record{}, fmt.Errorf("validate %s: filename ID %q does not match record ID %q", path, filenameID, record.ID)
	}
	return record, nil
}

func invalidWarning(path, message string) Warning {
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if !idPattern.MatchString(id) {
		id = ""
	}
	return Warning{Type: "invalid_record", Message: message, Path: path, MemoryID: id}
}
