package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

const (
	DefaultEmbeddingURL      = "http://127.0.0.1:11434"
	DefaultEmbeddingModel    = "all-minilm"
	DefaultSemanticThreshold = 0.60
)

type Config struct {
	Index           string          `toml:"index"`
	MemoryDirectory string          `toml:"memory_directory"`
	MemoryIndex     string          `toml:"memory_index"`
	Embedding       EmbeddingConfig `toml:"embedding"`
	Harnesses       map[string]bool `toml:"harnesses"`
	Sources         SourcesConfig   `toml:"sources"`
}

type EmbeddingConfig struct {
	URL       string  `toml:"url"`
	Model     string  `toml:"model"`
	Threshold float64 `toml:"threshold"`
}

type SourcesConfig struct {
	Pi       string `toml:"pi"`
	Codex    string `toml:"codex"`
	OpenCode string `toml:"opencode"`
	Claude   string `toml:"claude"`
}

func Default() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}

	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}

	return Config{
		Index:           filepath.Join(dataHome, "recall", "index.sqlite"),
		MemoryDirectory: filepath.Join(dataHome, "recall", "memories"),
		MemoryIndex:     filepath.Join(dataHome, "recall", "memory-index.sqlite"),
		Embedding: EmbeddingConfig{
			URL:       DefaultEmbeddingURL,
			Model:     DefaultEmbeddingModel,
			Threshold: DefaultSemanticThreshold,
		},
		Harnesses: map[string]bool{
			"pi":       true,
			"codex":    true,
			"opencode": true,
			"claude":   true,
		},
		Sources: SourcesConfig{
			Pi:     filepath.Join(home, ".pi", "agent", "sessions"),
			Codex:  filepath.Join(home, ".codex", "sessions"),
			Claude: filepath.Join(home, ".claude", "projects"),
		},
	}, nil
}

func LoadRecall() (Config, string, error) {
	cfg, err := Default()
	if err != nil {
		return Config{}, "", err
	}
	return loadWithConfigPath(cfg, recallConfigPath)
}

func loadWithConfigPath(cfg Config, pathResolver func() (string, error)) (Config, string, error) {
	path, err := pathResolver()
	if err != nil {
		return Config{}, "", err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, path, nil
	} else if err != nil {
		return Config{}, path, fmt.Errorf("inspect config %s: %w", path, err)
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, path, fmt.Errorf("read config %s: %w", path, err)
	}
	if cfg.Embedding.URL == "" {
		cfg.Embedding.URL = DefaultEmbeddingURL
	}
	if cfg.Embedding.Model == "" {
		cfg.Embedding.Model = DefaultEmbeddingModel
	}
	if cfg.Harnesses == nil {
		cfg.Harnesses = map[string]bool{}
	}
	if cfg.MemoryDirectory == "" {
		cfg.MemoryDirectory = DefaultMemoryDirectory()
	}
	if cfg.MemoryIndex == "" {
		cfg.MemoryIndex = DefaultMemoryIndex()
	}
	return cfg, path, nil
}

func recallConfigPath() (string, error) {
	if explicit := os.Getenv("RECALL_CONFIG"); explicit != "" {
		return expandHome(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "recall", "config.toml"), nil
}

func ExpandPaths(cfg *Config) error {
	var err error
	if cfg.Index, err = expandHome(cfg.Index); err != nil {
		return err
	}
	if cfg.MemoryDirectory != "" {
		cfg.MemoryDirectory, err = expandHome(cfg.MemoryDirectory)
		if err != nil {
			return fmt.Errorf("expand memory directory: %w", err)
		}
	}
	if cfg.MemoryIndex, err = expandHome(cfg.MemoryIndex); err != nil {
		return fmt.Errorf("expand memory index: %w", err)
	}
	for name, value := range map[string]*string{
		"pi": &cfg.Sources.Pi, "codex": &cfg.Sources.Codex,
		"opencode": &cfg.Sources.OpenCode, "claude": &cfg.Sources.Claude,
	} {
		if *value == "" {
			continue
		}
		*value, err = expandHome(*value)
		if err != nil {
			return fmt.Errorf("expand %s source: %w", name, err)
		}
	}
	if cfg.MemoryDirectory == "" {
		cfg.MemoryDirectory = DefaultMemoryDirectory()
	}
	if cfg.MemoryIndex == "" {
		cfg.MemoryIndex = DefaultMemoryIndex()
	}
	return nil
}

func DefaultMemoryDirectory() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "recall", "memories")
}

func DefaultMemoryIndex() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "recall", "memory-index.sqlite")
}

func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if len(path) > 1 && path[1] != '/' {
		return "", fmt.Errorf("unsupported home path %q", path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
