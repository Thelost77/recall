package cli

import (
	"context"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Thelost77/recall/internal/config"
)

type App struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getwd   func() (string, error)
	Version string
}

func New() *App {
	return &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getwd: os.Getwd, Version: "dev"}
}

func (a *App) defaults() {
	if a.Stdin == nil {
		a.Stdin = strings.NewReader("")
	}
	if a.Stdout == nil {
		a.Stdout = io.Discard
	}
	if a.Stderr == nil {
		a.Stderr = io.Discard
	}
	if a.Getwd == nil {
		a.Getwd = os.Getwd
	}
	if a.Version == "" {
		a.Version = "dev"
	}
}

func (a *App) RunAgentSessions(ctx context.Context, args []string) error {
	a.defaults()
	cfg, configPath, err := config.Load()
	if err != nil {
		return err
	}
	if err := config.ExpandPaths(&cfg); err != nil {
		return err
	}
	return a.runAgentSessions(ctx, cfg, configPath, args)
}

func (a *App) RunRecall(ctx context.Context, args []string) error {
	a.defaults()
	cfg, configPath, err := config.LoadRecall()
	if err != nil {
		return err
	}
	if err := config.ExpandPaths(&cfg); err != nil {
		return err
	}
	return a.runRecall(ctx, cfg, configPath, args)
}

func BuildVersion(version string) string {
	if version != "" && version != "dev" {
		return strings.TrimPrefix(version, "v")
	}
	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}
