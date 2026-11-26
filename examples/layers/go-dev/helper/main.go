// Command go-dev-helper is the go-dev example layer's own program.
//
// ADR 0017 makes an example layer self-contained: its helper is a module of its
// own, compiled by the layer's pinned `golang:` stage and mounted read-only into
// the RUN that uses it, because an example build has no `--build-context` and
// nothing from the host or from ./internal can reach it.
//
// This slice (#249) ships one verb. `check` reads the committed gopls closure and
// proves it offline: the same five files, the same rules and the same printed
// release as examples/layers/go-dev/gopls_closure.py, which stays live as the
// oracle until #254 deletes it. It runs no Go command and makes no network
// request, which is why the recipe runs it with `--network=none` and only the
// binary and the data mounted.
//
// The Python reader's `current` verb is not ported (#188 R-b): its only reader in
// this repository is the maintenance stager's own `current`, a different program.
// `install`, `verify-installed` and the writer are other slices (#250, #251,
// #253); until the last of them the Python module is the only installer.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
)

// prefix is the frozen diagnostic prefix the Python reader prints. The shell
// suites, the pins suite and the native harness match on it.
const prefix = "gopls-closure"

type arguments struct {
	Check    *checkCommand `arg:"subcommand:check" help:"verify the committed gopls closure offline"`
	LogLevel string        `arg:"--log-level" default:"warn" placeholder:"LEVEL" help:"debug, info, warn or error; diagnostics go to stderr"`
}

func (arguments) Description() string {
	return "go-dev example layer helper. Runs from the layer's own build stages."
}

type checkCommand struct {
	DataDir string `arg:"positional,required" placeholder:"DATA_DIR" help:"the committed closure directory"`
	// Pointers, so an explicitly empty assertion fails instead of meaning "unset":
	// the caller that passes the flag is asserting a version.
	ExpectGopls *string `arg:"--expect-gopls" placeholder:"EXACT" help:"assert this exact vX.Y.Z release"`
	ExpectGo    *string `arg:"--expect-go" placeholder:"EXACT" help:"assert this exact X.Y.Z Go version"`
}

func (checkCommand) Description() string {
	return "Verify the committed closure offline."
}

func main() { os.Exit(run(os.Args[1:])) }

// run maps every outcome to the Python reader's status: 0 success, 1 refusal, and
// 2 for a malformed invocation of a verb that exists here.
func run(args []string) int {
	var parsed arguments
	parser, err := arg.NewParser(arg.Config{Program: "go-dev-helper", IgnoreEnv: true}, &parsed)
	if err != nil {
		return refuse(err.Error())
	}
	if err := parser.Parse(args); err != nil {
		if len(parser.SubcommandNames()) > 0 {
			return refuseUsage()
		}
		return refuse("usage: " + usageText)
	}
	configureLogging(parsed.LogLevel)
	switch {
	case parsed.Check != nil:
		return runCheck(parsed.Check)
	default:
		return refuse("usage: " + usageText)
	}
}

// runCheck reads the closure and prints the release the consumer parses.
func runCheck(command *checkCommand) int {
	request, err := command.request()
	if err != nil {
		return refuseError(err)
	}
	source, err := closure.DirSource(command.DataDir)
	if err != nil {
		return refuseError(err)
	}
	data, err := closure.Load(context.Background(), request, source)
	if err != nil {
		return refuseError(err)
	}
	fmt.Println(data.Pin.Gopls)
	return 0
}

// request turns the flag spellings into the loader's validated assertions.
func (command *checkCommand) request() (closure.Request, error) {
	var request closure.Request
	if command.ExpectGopls != nil {
		release, err := closure.ParseReleaseVersion(*command.ExpectGopls, "--expect-gopls")
		if err != nil {
			return closure.Request{}, err
		}
		request.ExpectGopls = &release
	}
	if command.ExpectGo != nil {
		version, err := closure.ParseGoVersion(*command.ExpectGo, "--expect-go")
		if err != nil {
			return closure.Request{}, err
		}
		request.ExpectGo = &version
	}
	return request, nil
}

// configureLogging installs the one diagnostic sink: a text handler on stderr.
// Contract lines are printed separately and never through this logger, because a
// logger quotes and escapes.
func configureLogging(level string) {
	parsed := slog.LevelWarn
	switch level {
	case "debug":
		parsed = slog.LevelDebug
	case "info":
		parsed = slog.LevelInfo
	case "warn", "":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed})))
}

// usageText is the one usage line. The shell suites match on it.
const usageText = "go-dev-helper {check} ARGS"

// refuseError reports one `gopls-closure: ` line and returns the refusal status:
// the Python reader's contract line and its status.
func refuseError(err error) int {
	fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, err)
	return 1
}

func refuse(message string) int {
	fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, message)
	return 1
}

// refuseUsage reports a malformed invocation of a verb with argparse's status 2.
func refuseUsage() int {
	fmt.Fprintf(os.Stderr, "%s: usage: %s\n", prefix, usageText)
	return 2
}
