package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Thelost77/recall/internal/config"
)

func TestRunSearchLexicalOnlyDoesNotUseEmbedder(t *testing.T) {
	cfg := config.Config{
		Index: filepath.Join(t.TempDir(), "index.sqlite"),
	}

	if err := runSearch(context.Background(), cfg, []string{"--lexical-only", "missing"}); err != nil {
		t.Fatal(err)
	}
}
