package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultSemanticThreshold(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.Threshold != 0.60 {
		t.Fatalf("semantic threshold = %f, want 0.60", cfg.Embedding.Threshold)
	}
	if cfg.MemoryDirectory == "" || cfg.MemoryIndex == "" {
		t.Fatalf("memory defaults are empty: %#v", cfg)
	}
	if filepath.Base(filepath.Dir(cfg.Index)) != "recall" {
		t.Fatalf("session index default = %q, want under recall/", cfg.Index)
	}
}

func TestRecallConfigurationPrecedence(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("RECALL_CONFIG", "")
	recallPath := filepath.Join(configHome, "recall", "config.toml")
	if err := os.MkdirAll(filepath.Dir(recallPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recallPath, []byte("index = 'recall.sqlite'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, used, err := LoadRecall()
	if err != nil {
		t.Fatal(err)
	}
	if used != recallPath || cfg.Index != "recall.sqlite" {
		t.Fatalf("recall config = %q %#v", used, cfg)
	}
	explicitRecall := filepath.Join(root, "explicit-recall.toml")
	if err := os.WriteFile(explicitRecall, []byte("index = 'explicit-recall.sqlite'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECALL_CONFIG", explicitRecall)
	cfg, used, err = LoadRecall()
	if err != nil {
		t.Fatal(err)
	}
	if used != explicitRecall || cfg.Index != "explicit-recall.sqlite" {
		t.Fatalf("explicit recall config = %q %#v", used, cfg)
	}
}
