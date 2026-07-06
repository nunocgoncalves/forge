package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/forge/internal/config"
)

func loadConfig(cmd *cobra.Command) (*config.Cluster, error) {
	path, _ := cmd.Flags().GetString("config")
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}
	return cfg, nil
}

func newLogger(cmd *cobra.Command) *slog.Logger {
	levelStr, _ := cmd.Flags().GetString("log-level")
	format, _ := cmd.Flags().GetString("log-format")

	var level slog.Level
	switch levelStr {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
