package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Thelost77/agent-sessions/internal/cli"
	"github.com/Thelost77/agent-sessions/internal/config"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	app := cli.New()
	app.Version = version
	return app.RunAgentSessions(ctx, args)
}

func runSearch(ctx context.Context, cfg config.Config, args []string) error {
	app := cli.New()
	app.Version = version
	return app.RunSessionSearch(ctx, cfg, args, true, "")
}

func buildVersion() string { return cli.BuildVersion(version) }
