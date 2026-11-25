// Command pagewire is the pagewire control plane (serve), the device-pod
// agent (agent probe|fetch|run), the config checker (validate), the Docker
// health check (healthcheck) and version.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: pagewire <command> [flags]

commands:
  serve                  run the control plane
  agent probe|fetch|run  device-pod agent
  validate <file>...     check config files
  healthcheck            GET /healthz on the local listener
  version                print the version
`

func main() {
	if err := setLogLevel(env("PAGEWIRE_LOG_LEVEL", "info")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pagewire:", err)
		os.Exit(2)
	}
	// JSON inside Kubernetes, text otherwise.
	opts := &slog.HandlerOptions{Level: &logLevel}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches a subcommand and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, rest, nil)
	case "agent":
		if len(rest) > 0 && isHelp(rest[0]) {
			_, _ = io.WriteString(stderr, agentUsage)
			return 2
		}
		err = agentCmd(ctx, rest)
	case "validate":
		return validate(rest, stdout, stderr)
	case "healthcheck":
		err = healthcheck(ctx, rest)
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case "-h", "-help", "--help", "help":
		_, _ = io.WriteString(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "pagewire: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 2
	default:
		slog.Error("command failed", "command", cmd, "err", err)
		return 1
	}
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help" || arg == "help"
}

// logLevel is the default logger's level; serve's --log-level changes it.
var logLevel slog.LevelVar

func setLogLevel(level string) error {
	if err := logLevel.UnmarshalText([]byte(strings.TrimSpace(level))); err != nil {
		return fmt.Errorf("log level %q: %w", level, err)
	}
	return nil
}

// env returns the environment variable name, or def when it is unset or empty.
func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// strList is a repeatable string flag.
type strList []string

func (s *strList) String() string     { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error { *s = append(*s, v); return nil }
