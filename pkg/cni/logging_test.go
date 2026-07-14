package cni

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		" Info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelWarn, // default
		"bogus":   slog.LevelWarn, // unrecognized falls back
	}
	for in, want := range cases {
		assert.Equalf(t, want, parseLogLevel(in), "parseLogLevel(%q)", in)
	}
}

func TestConfigureLoggingSetsLevel(t *testing.T) {
	// Preserve and restore the global default logger the test mutates.
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	ctx := context.Background()

	ConfigureLogging(&PluginConf{LogLevel: "debug"})
	assert.True(t, slog.Default().Enabled(ctx, slog.LevelDebug),
		"debug should be enabled when logLevel=debug")

	// Default (unset) is warn, so debug/info are filtered.
	ConfigureLogging(&PluginConf{})
	assert.False(t, slog.Default().Enabled(ctx, slog.LevelInfo),
		"info should be filtered at the default warn level")
	assert.True(t, slog.Default().Enabled(ctx, slog.LevelWarn),
		"warn should be enabled at the default level")
}
