package cni

import (
	"log/slog"
	"os"
	"strings"
)

// parseLogLevel maps a logLevel config string to a slog.Level. Matching is
// case-insensitive; empty or unrecognized values fall back to warn, the
// historical default.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// validLogLevel reports whether s is an accepted logLevel value. The empty
// string is not accepted here; LoadConfig treats "unset" separately so it can
// default without flagging it as invalid.
func validLogLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "info", "warn", "warning", "error":
		return true
	default:
		return false
	}
}

// ConfigureLogging installs the default slog logger at the level from the
// plugin config (conf.LogLevel; default warn). A CNI plugin logs to stderr,
// which the runtime (kubelet) captures.
func ConfigureLogging(conf *PluginConf) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(conf.LogLevel),
	})))
}
