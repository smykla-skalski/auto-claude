package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/marcin-skalski/auto-claude/internal/claude"
	"github.com/marcin-skalski/auto-claude/internal/config"
	"github.com/marcin-skalski/auto-claude/internal/daemon"
	"github.com/marcin-skalski/auto-claude/internal/git"
	"github.com/marcin-skalski/auto-claude/internal/github"
	"github.com/marcin-skalski/auto-claude/internal/logging"
	"github.com/marcin-skalski/auto-claude/internal/tui"
	"github.com/mattn/go-isatty"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	noTUI := flag.Bool("no-tui", false, "disable TUI mode")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Auto-detect TUI capability
	enableTUI := !*noTUI && os.Getenv("AUTO_CLAUDE_TUI") != "0" &&
		isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())

	logger, err := logging.SetupLogger(cfg.LogFile, cfg.Log.Level, enableTUI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup logger: %v\n", err)
		os.Exit(1)
	}

	gh := github.NewClient(logger)
	cl := claude.NewClient(cfg.Claude.Model, logger)
	g := git.NewClient(cfg.Workdir, logger)

	d := daemon.New(cfg, gh, cl, g, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if enableTUI {
		// TUI mode: run daemon in background, TUI in foreground
		errCh := make(chan error, 1)
		go func() {
			logger.Info("auto-claude daemon starting in background", "config", *configPath)
			if err := d.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("daemon error", "err", err)
				errCh <- err
			}
		}()

		m := tui.NewModel(d, cfg.TUI.RefreshInterval)
		p := tea.NewProgram(m)

		// Exit if daemon fails immediately
		go func() {
			if err := <-errCh; err != nil {
				p.Send(tea.Quit())
			}
		}()

		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Headless mode
		logger.Info("auto-claude starting (headless)", "config", *configPath)
		if err := d.Run(ctx); err != nil {
			logger.Error("daemon error", "err", err)
			os.Exit(1)
		}
	}
}
