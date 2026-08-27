package adapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Thelost77/recall/internal/model"
)

func stableKey(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{':'})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileSources(ctx context.Context, harness, root string, skip func(string) bool) ([]model.Source, error) {
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s source root %s does not exist", harness, root)
	} else if err != nil {
		return nil, fmt.Errorf("inspect %s source root %s: %w", harness, root, err)
	}

	var sources []model.Source
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && skip != nil && skip(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".jsonl" || (skip != nil && skip(path)) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		sources = append(sources, model.Source{
			Key:     stableKey(harness, path),
			Harness: harness,
			Path:    path,
			Version: fmt.Sprintf("json-v2:%d:%d", info.Size(), info.ModTime().UnixNano()),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover %s sessions: %w", harness, err)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func decodeJSONValues(path string, visit func(json.RawMessage) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 128*1024)
	decoder := json.NewDecoder(reader)
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(fmt.Sprint(err), "unexpected EOF") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode JSON near byte %d: %w", decoder.InputOffset(), err)
		}
		if err := visit(raw); err != nil {
			return err
		}
	}
}

func decodeJSONLinesTolerant(path string, visit func(json.RawMessage) error) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 128*1024)
	var warnings []string
	lineNumber := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		lineNumber++
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			if !json.Valid(trimmed) {
				if errors.Is(readErr, io.EOF) {
					return warnings, nil
				}
				warnings = append(warnings, fmt.Sprintf("ignored malformed JSONL line %d", lineNumber))
			} else if err := visit(json.RawMessage(trimmed)); err != nil {
				return warnings, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return warnings, nil
		}
		if readErr != nil {
			return warnings, readErr
		}
	}
}

func contentText(value any, acceptedTypes map[string]bool) string {
	switch content := value.(type) {
	case string:
		return strings.TrimSpace(content)
	case []any:
		var texts []string
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := block["type"].(string)
			if len(acceptedTypes) > 0 && !acceptedTypes[typeName] {
				continue
			}
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				texts = append(texts, strings.TrimSpace(text))
			}
		}
		return strings.TrimSpace(strings.Join(texts, "\n\n"))
	default:
		return ""
	}
}

func parseISO(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	result, _ := time.Parse(time.RFC3339Nano, value)
	return result
}

func unixMillis(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

func updateBounds(started, updated *time.Time, candidate time.Time) {
	if candidate.IsZero() {
		return
	}
	if started.IsZero() || candidate.Before(*started) {
		*started = candidate
	}
	if updated.IsZero() || candidate.After(*updated) {
		*updated = candidate
	}
}

func firstLineTitle(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "Untitled session"
	}
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	const maxRunes = 100
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > maxRunes {
		return string(runes[:maxRunes-1]) + "…"
	}
	return string(runes)
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}
