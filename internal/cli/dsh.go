package cli

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/dsh"
	"github.com/gregwebs/agent-vm-images/internal/dshupgrade"
	"github.com/gregwebs/agent-vm-images/internal/pnpmlock"
)

func errorOf(text string) error { return errors.New(text) }

// runDshLockUpdate is the argv port of the retired dsh lock-update adapter,
// with `--json` added.
func (p *process) runDshLockUpdate(args []string) int {
	if len(args) < 3 {
		return p.nodeUsage(2, dshLockUpdateUsage)
	}
	refresh, jsonOut := false, false
	seen := map[string]bool{}
	for _, option := range args[3:] {
		if seen[option] {
			return p.nodeUsage(2, dshLockUpdateUsage)
		}
		seen[option] = true
		switch option {
		case "--refresh-lock":
			refresh = true
		case "--json":
			jsonOut = true
		default:
			return p.nodeUsage(2, dshLockUpdateUsage)
		}
	}
	return p.nodeCommand(dshLockUpdatePrefix, func() (string, error) {
		mode := pnpmlock.NoRefresh
		if refresh {
			mode = pnpmlock.RefreshLock
		}
		if !jsonOut {
			return pnpmlock.CompareDshUpdateDiagnostic(pnpmlock.UpdateRequest{
				Before:  pnpmlock.Prefix(args[0]),
				After:   pnpmlock.Prefix(args[1]),
				Refresh: mode,
			}, args[2])
		}
		comparison, _, err := pnpmlock.CompareDshUpdateArgument(pnpmlock.UpdateRequest{
			Before:  pnpmlock.Prefix(args[0]),
			After:   pnpmlock.Prefix(args[1]),
			Refresh: mode,
		}, args[2])
		if err != nil {
			return "", err
		}
		return string(comparison.AppendJS(nil)) + "\n", nil
	})
}

// runDshCordisOverrides is the argv port of the retired Cordis derivation
// adapter.
func (p *process) runDshCordisOverrides(args []string) int {
	prefix := ""
	if len(args) > 0 {
		prefix = args[0]
	}
	options := args[min(len(args), 1):]
	flags := map[string]string{}
	usage := prefix == ""
	for index := 0; index < len(options); index += 2 {
		flag := options[index]
		value := ""
		if index+1 < len(options) {
			value = options[index+1]
		}
		if !knownDerivationFlag(flag) || hasFlag(flags, flag) || value == "" || strings.HasPrefix(value, "--") {
			usage = true
		}
		flags[flag] = value
	}
	if hasFlag(flags, "--propose") && hasFlag(flags, "--cordis-overrides") {
		usage = true
	}
	if usage {
		return p.nodeUsage(2, dshDerivationUsage)
	}
	return p.nodeCommand(dshDerivationPrefix, func() (string, error) {
		cwd, err := syscall.Getwd()
		if err != nil {
			return "", err
		}
		request := dshupgrade.DeriveRequest{
			Tentative: pnpmlock.Prefix(prefix),
			Approved:  flags["--cordis-overrides"],
			Propose:   flags["--propose"],
			Cwd:       cwd,
		}
		if before, ok := flags["--before"]; ok {
			prefix := pnpmlock.Prefix(before)
			request.Before = &prefix
		}
		result, err := dshupgrade.Derive(request)
		if err != nil {
			return "", err
		}
		return string(result.AppendJS(nil)) + "\n", nil
	})
}

func knownDerivationFlag(flag string) bool {
	switch flag {
	case "--before", "--propose", "--cordis-overrides":
		return true
	default:
		return false
	}
}

func hasFlag(flags map[string]string, flag string) bool {
	_, ok := flags[flag]
	return ok
}

// runDshUpgradeStage is the argv port of the retired developer staging helper.
// It has no usage path: every failure is status 1 with the `dsh upgrade: ` prefix.
func (p *process) runDshUpgradeStage(args []string) int {
	positional := func(index int) string {
		if index < len(args) {
			return args[index]
		}
		return ""
	}
	action, before, stage, argument, extra := positional(0), positional(1), positional(2), positional(3), positional(4)
	return p.nodeCommand(dshUpgradePrefix, func() (string, error) {
		// The retired adapter read the committed project before dispatching, so a
		// broken prefix wins over an unknown action.
		if len(args) < 2 {
			return "", errorOf("missing argument: before")
		}
		if _, err := pnpmlock.ReadDshProject(pnpmlock.Prefix(before)); err != nil {
			return "", err
		}
		request := dshupgrade.StageRequest{
			Before:   pnpmlock.Prefix(before),
			Stage:    stage,
			Argument: argument,
			Extra:    extra,
			ExtraSet: extra != "",
		}
		switch action {
		case "tentative":
			if request.ExtraSet {
				return "", errorOf("expected target exact selection")
			}
			return "", dshupgrade.StageTentative(request)
		case "final":
			return "", dshupgrade.StageFinal(request)
		case "check-final":
			report, err := dshupgrade.CheckFinal(request)
			if err != nil {
				return "", err
			}
			return string(report.AppendJS(nil)) + "\n", nil
		default:
			return "", errorOf("unknown developer staging action")
		}
	})
}

// dshDiagnostics names the byte prefixes a dsh subcommand's predecessors wrote.
// They are distinct per verb because the retired programs chose them: the probe
// prefixed a usage refusal with its own name and an operational one with its log
// section, while the install and preparation hooks used their script names and the
// gate indented every line under its own header.
type dshDiagnostics struct {
	Usage     string
	Refusal   string
	Interrupt string
}

var (
	dshProbeDiagnostics   = dshDiagnostics{Usage: "dsh-probe: ", Refusal: "dsh-runtime: web: ", Interrupt: "dsh-runtime: web: interrupted"}
	dshInstallDiagnostics = dshDiagnostics{Usage: "install-dsh.sh: ", Refusal: "install-dsh.sh: "}
	dshPrepareDiagnostics = dshDiagnostics{Usage: "prepare-lock.sh: ", Refusal: "prepare-lock.sh: "}
	dshVerifyDiagnostics  = dshDiagnostics{Usage: "  ", Refusal: "  "}
)

// dshFailure reports one dsh refusal with the status its predecessor used: a
// malformed argument is usage (2), a well-formed request the program refuses is 1,
// and a child that failed keeps its own status and its own stderr.
func (p *process) dshFailure(diagnostics dshDiagnostics, err error) int {
	var usage *dsh.Usage
	if errors.As(err, &usage) {
		return p.stderrStatus(2, diagnostics.Usage+usage.Text+"\n")
	}
	var interrupted *dsh.Interrupted
	if errors.As(err, &interrupted) {
		if diagnostics.Interrupt != "" {
			if writeErr := p.err.writeString(diagnostics.Interrupt + "\n"); writeErr != nil {
				return streamFailureStatus
			}
		}
		return interrupted.Status
	}
	var refusal *dsh.Refusal
	if errors.As(err, &refusal) {
		return p.stderrStatus(1, diagnostics.Refusal+refusal.Text+"\n")
	}
	var child *dsh.ChildFailure
	if errors.As(err, &child) {
		// The child's own bytes, verbatim: the shell inherited them, so this adds
		// neither a prefix nor a newline.
		if child.Stderr != "" {
			if writeErr := p.err.writeString(child.Stderr); writeErr != nil {
				return streamFailureStatus
			}
		}
		return child.Status
	}
	var silent *dsh.Silent
	if errors.As(err, &silent) {
		// The child reported its own refusal and the predecessor added nothing.
		return silent.Status
	}
	return p.stderrStatus(1, diagnostics.Refusal+err.Error()+"\n")
}

// dshProbeUsage is the bounded web-readiness probe. Every fixture input is a flag:
// the child environment entries the probe hands the web process and the exec temp
// parent are named per invocation instead of being inherited from this process's
// environment, so production cannot be handed one by accident.
type dshProbeUsage struct {
	Source            string   `arg:"--source" placeholder:"DIR" help:"the dsh tools directory carrying project.py"`
	Prefix            string   `arg:"--prefix" placeholder:"DIR" help:"the installed payload to probe"`
	Command           string   `arg:"--command" placeholder:"PATH" help:"the installed command to run"`
	ScratchRoot       string   `arg:"--scratch-root" placeholder:"DIR" help:"the invocation-owned scratch root"`
	ReadinessSeconds  string   `arg:"--readiness-seconds" placeholder:"N" help:"readiness deadline override (fixture only)"`
	EscalationSeconds string   `arg:"--escalation-seconds" placeholder:"N" help:"TERM-to-KILL escalation override (fixture only)"`
	FixtureChildEnv   []string `arg:"--fixture-child-env,separate" placeholder:"NAME=VALUE" help:"one extra child environment entry (fixture only)"`
	FixtureExecTmp    string   `arg:"--fixture-exec-tmpdir" placeholder:"DIR" help:"the exec temp parent (fixture only)"`
	SelfTest          bool     `arg:"--self-test" help:"prove the ownership proof degrades, then exit"`
}

func (dshProbeUsage) Description() string {
	return "Probe the installed dsh web frontend under a bounded deadline."
}

func (p *process) runDshProbe(args []string) int {
	var usage dshProbeUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract dsh-probe"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	if usage.SelfTest {
		// The predecesor accepted `--self-test` only as the whole argv, so a caller
		// that also named a payload was reporting a mistake, not asking for both.
		if usage.Source != "" || usage.Prefix != "" || usage.Command != "" || usage.ScratchRoot != "" ||
			usage.ReadinessSeconds != "" || usage.EscalationSeconds != "" ||
			len(usage.FixtureChildEnv) > 0 || usage.FixtureExecTmp != "" {
			return p.stderrStatus(2, dshProbeDiagnostics.Usage+dshProbeUsageText+"\n")
		}
		if err := dsh.SelfTest(); err != nil {
			return p.dshFailure(dshProbeDiagnostics, err)
		}
		return p.stdoutStatus("probe-dsh: ownership degradation self-test PASS\n")
	}
	if usage.Source == "" || usage.Prefix == "" || usage.Command == "" || usage.ScratchRoot == "" {
		return p.stderrStatus(2, dshProbeDiagnostics.Usage+dshProbeUsageText+"\n")
	}
	options := dsh.ProbeOptions{
		Source:            usage.Source,
		Prefix:            usage.Prefix,
		Command:           usage.Command,
		ScratchRoot:       usage.ScratchRoot,
		FixtureChildEnv:   usage.FixtureChildEnv,
		FixtureExecTmpdir: usage.FixtureExecTmp,
	}
	if options.Readiness, err = probeBound(usage.ReadinessSeconds); err != nil {
		return p.dshFailure(dshProbeDiagnostics, err)
	}
	if options.Escalation, err = probeBound(usage.EscalationSeconds); err != nil {
		return p.dshFailure(dshProbeDiagnostics, err)
	}
	report, err := dsh.Probe(context.Background(), options)
	if err != nil {
		return p.dshFailure(dshProbeDiagnostics, err)
	}
	return p.stdoutStatus(string(report))
}

const probeBoundText = "probe bounds must be positive and at most the production deadline"

// probeBound parses one optional seconds bound. Nil means "keep the production
// value"; a named bound must be positive and no longer than the production
// deadline, so a caller cannot buy a slower gate.
func probeBound(text string) (*time.Duration, error) {
	if text == "" {
		return nil, nil
	}
	seconds, err := strconv.ParseFloat(text, 64)
	// NaN and both infinities parse, and every comparison against NaN is false, so a
	// finiteness test is what keeps a bound the deadline arithmetic cannot express from
	// passing as "positive and small enough".
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return nil, &dsh.Usage{Text: probeBoundText}
	}
	bound := time.Duration(seconds * float64(time.Second))
	if seconds <= 0 || bound > dsh.DefaultReadiness {
		return nil, &dsh.Usage{Text: probeBoundText}
	}
	return &bound, nil
}

const dshProbeUsageText = "usage: recipe-contract dsh-probe --source DIR --prefix PREFIX" +
	" --command COMMAND --scratch-root OWNED_ROOT [--readiness-seconds N]" +
	" [--escalation-seconds N] [--fixture-exec-tmpdir DIR] [--fixture-child-env NAME=VALUE]..."

// dshPaths are the flags every dsh hook shares: the sources it reads and the
// authorized composed directory it executes from.
type dshPaths struct {
	Source   string `arg:"--source,required" placeholder:"DIR" help:"the dsh tools directory"`
	Contract string `arg:"--contract,required" placeholder:"DIR" help:"the authorized composed helper directory"`
}

// dshInstallUsage is the owning install hook. Every slot the container exports is a
// flag here, so the entry point is the only place the environment is read.
type dshInstallUsage struct {
	dshPaths
	Prefix      string `arg:"--prefix,required" placeholder:"DIR" help:"the payload destination"`
	Command     string `arg:"--command,required" placeholder:"PATH" help:"the command the image links to the payload"`
	DshVersion  string `arg:"--dsh-version" placeholder:"EXACT" help:"the layer's requested dsh slot"`
	PnpmVersion string `arg:"--pnpm-version" placeholder:"EXACT" help:"the layer's requested pnpm slot"`
}

func (dshInstallUsage) Description() string {
	return "Install the reviewed dsh inputs into an image layer."
}

func (p *process) runDshInstall(args []string) int {
	var usage dshInstallUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract dsh-install"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	options := dsh.InstallOptions{
		Source:      usage.Source,
		Contract:    usage.Contract,
		Prefix:      usage.Prefix,
		Command:     usage.Command,
		DshVersion:  usage.DshVersion,
		PnpmVersion: usage.PnpmVersion,
		Out:         p.streams.Out,
		Err:         p.streams.Err,
	}
	if err := dsh.Install(context.Background(), options); err != nil {
		return p.dshFailure(dshInstallDiagnostics, err)
	}
	return 0
}

// dshPrepareUsage is the preparation driver: three positional slots and at most one
// option, which is why the argv grammar belongs to the driver rather than to the
// flag parser. `--refresh-lock` is the driver's own word, so it reaches it unchanged
// and an unknown one is that grammar's usage refusal.
type dshPrepareUsage struct {
	Source    string
	Contract  string
	Arguments []string
}

func (p *process) runDshPrepareLock(args []string) int {
	var usage dshPrepareUsage
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--source", "--contract":
			value := ""
			if index+1 < len(args) {
				value = args[index+1]
				index++
			}
			if value == "" {
				return p.stderrStatus(2, dshPrepareUsageText)
			}
			if args[index-1] == "--source" {
				usage.Source = value
			} else {
				usage.Contract = value
			}
		default:
			usage.Arguments = append(usage.Arguments, args[index])
		}
	}
	if usage.Source == "" || usage.Contract == "" {
		return p.stderrStatus(2, dshPrepareUsageText)
	}
	options := dsh.PrepareOptions{
		Source:    usage.Source,
		Contract:  usage.Contract,
		Arguments: usage.Arguments,
		Out:       p.streams.Out,
		Err:       p.streams.Err,
	}
	if err := dsh.PrepareLock(context.Background(), options); err != nil {
		return p.dshFailure(dshPrepareDiagnostics, err)
	}
	return 0
}

const dshPrepareUsageText = "dsh-prepare-lock: usage: recipe-contract dsh-prepare-lock --source DIR" +
	" --contract DIR PREFIX DSH_VERSION PNPM_VERSION [--refresh-lock]\n"

// dshVerifyUsage is the installed-payload gate.
type dshVerifyUsage struct {
	dshPaths
	Prefix      string `arg:"--prefix,required" placeholder:"DIR" help:"the installed payload"`
	Command     string `arg:"--command,required" placeholder:"PATH" help:"the installed command"`
	StatusDir   string `arg:"--status-dir,required" placeholder:"DIR" help:"the install-status directory"`
	DshVersion  string `arg:"--dsh-version" placeholder:"EXACT" help:"the layer's requested dsh slot"`
	PnpmVersion string `arg:"--pnpm-version" placeholder:"EXACT" help:"the layer's requested pnpm slot"`
}

func (dshVerifyUsage) Description() string {
	return "Gate an installed dsh payload on reports, runtime behaviour and access."
}

func (p *process) runDshVerify(args []string) int {
	var usage dshVerifyUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract dsh-verify"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	options := dsh.VerifyOptions{
		Source:    usage.Source,
		Contract:  usage.Contract,
		Prefix:    usage.Prefix,
		Command:   usage.Command,
		StatusDir: usage.StatusDir,
		DshSlot:   usage.DshVersion,
		PnpmSlot:  usage.PnpmVersion,
		Access:    p.env.Environment,
		Out:       p.streams.Out,
		Err:       p.streams.Err,
	}
	if err := dsh.Verify(context.Background(), options); err != nil {
		return p.dshFailure(dshVerifyDiagnostics, err)
	}
	return 0
}
