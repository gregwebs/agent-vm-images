package main

// `avm-pin gopls`: the maintainer writer for the go-dev example's committed gopls
// closure (#252, slice 4a of #188). It resolves one exact release with the pinned Go,
// cross-builds both Linux targets offline, and writes a candidate directory; the
// publisher that copies it into the checkout is still script/dev/upgrade-gopls.sh
// (slice 4b), so this verb publishes nothing.
//
// The rules are not here. They are in the layer's own `closure` package, which the
// image's helper compiles and this binary imports through the root go.mod replace, so
// the writer and the image cannot disagree about what a pin means. The resolution
// commands — the only ones that may change a module graph — are writer-only, in
// internal/goplsstage.
//
// The diagnostic prefix is frozen at `gopls-closure-update: `, the prefix of the
// Python stager this ports: the shell wrapper and both suites match it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
	"github.com/gregwebs/agent-vm-images/internal/goplsstage"
)

type goplsCommand struct {
	Stage   *goplsStageCommand   `arg:"subcommand:stage" help:"resolve one exact gopls release and write a candidate directory"`
	Check   *goplsCheckCommand   `arg:"subcommand:check" help:"verify a committed closure directory offline"`
	Current *goplsCurrentCommand `arg:"subcommand:current" help:"print the selection a committed closure names"`
	Compare *goplsCompareCommand `arg:"subcommand:compare" help:"bind a staged candidate to a retained proposal"`
	// The fixture grammar is embedded: in the shipped build the type is empty, so the
	// parser has no fixture flag to accept or to refuse (see gopls_shipped.go).
	goplsFixtureArguments
}

func (goplsCommand) Description() string {
	return "Stage the go-dev example's gopls closure. Never in CI."
}

type goplsStageCommand struct {
	Gopls      string `arg:"--gopls,required" placeholder:"vX.Y.Z" help:"the exact gopls release to stage"`
	Go         string `arg:"--go,required" placeholder:"X.Y.Z" help:"the layer's pinned Go version"`
	Current    string `arg:"--current,required" placeholder:"DIR" help:"the committed closure directory this run derives from"`
	Out        string `arg:"--out,required" placeholder:"DIR" help:"a new directory for the candidate"`
	Dockerfile string `arg:"--dockerfile,required" placeholder:"FILE" help:"the layer Dockerfile whose ARG assertion is rewritten"`
	ReviewedOn string `arg:"--review-determination" placeholder:"YYYY-MM-DD" help:"the day a human reviewed the new scope"`
	Reviewer   string `arg:"--reviewer" placeholder:"NAME" help:"the reviewer who supplied that review"`
}

func (goplsStageCommand) Description() string {
	return "Resolve, prove and stage the closure; publish nothing."
}

type goplsCheckCommand struct {
	DataDir string `arg:"positional,required" placeholder:"DATA_DIR" help:"the committed closure directory"`
}

func (goplsCheckCommand) Description() string {
	return "Verify the committed closure offline and print its release."
}

type goplsCurrentCommand struct {
	DataDir string `arg:"positional,required" placeholder:"DATA_DIR" help:"the committed closure directory"`
}

func (goplsCurrentCommand) Description() string {
	return "Print the gopls release and Go version a committed closure names."
}

type goplsCompareCommand struct {
	Proposal string `arg:"positional,required" placeholder:"PROPOSAL_DIR" help:"the retained proposal directory"`
	Staged   string `arg:"positional,required" placeholder:"STAGED_DIR" help:"this run's staged directory"`
}

func (goplsCompareCommand) Description() string {
	return "Refuse unless the staged bytes equal the retained proposal."
}

// runGopls dispatches the four verbs. A refusal is status 1, a malformed invocation is
// status 2, and the two CI cases are the statuses the source writer used.
func runGopls(command *goplsCommand) int {
	seams, err := command.seams()
	if err != nil {
		return refuseGopls(1, err.Error())
	}
	if seams.Requested {
		for _, name := range seams.Names {
			fmt.Println(name)
		}
		return 0
	}
	environment, err := command.environment()
	if err != nil {
		return refuseGopls(2, err.Error())
	}
	switch {
	case command.Check != nil:
		data, err := loadGopls(command.Check.DataDir)
		if err != nil {
			return refuseGopls(1, err.Error())
		}
		fmt.Println(data.Pin.Gopls)
		return 0
	case command.Current != nil:
		data, err := loadGopls(command.Current.DataDir)
		if err != nil {
			return refuseGopls(1, err.Error())
		}
		fmt.Printf("gopls=%s\n", data.Pin.Gopls)
		fmt.Printf("go=%s\n", data.Pin.Go)
		return 0
	case command.Compare != nil:
		if err := compareProposal(command.Compare.Proposal, command.Compare.Staged); err != nil {
			return refuseGopls(1, err.Error())
		}
		fmt.Println("proposal bound: the staged bytes equal the retained proposal")
		return 0
	case command.Stage != nil:
		return runGoplsStage(command.Stage, command.goplsFixtureArguments, environment)
	default:
		fmt.Fprintln(os.Stderr, "usage: avm-pin gopls {stage,check,current,compare} ARGS")
		return 2
	}
}

// loadGopls is the one read both reader verbs use: the layer's loader over the
// committed bytes, so the stager's own verbs are a projection rather than a second
// reader.
func loadGopls(directory string) (*closure.Data, error) {
	source, err := closure.DirSource(directory)
	if err != nil {
		return nil, err
	}
	return closure.Load(context.Background(), closure.Request{}, source)
}

// runGoplsStage resolves and stages one candidate.
func runGoplsStage(command *goplsStageCommand, seams goplsFixtureArguments,
	environment gotool.Environment) int {
	for _, path := range []string{command.Current, command.Out, command.Dockerfile} {
		if !filepath.IsAbs(path) {
			return refuseGopls(2, "every path must be absolute: "+path)
		}
	}
	release, err := closure.ParseReleaseVersion(command.Gopls, "--gopls")
	if err != nil {
		return refuseGopls(1, err.Error())
	}
	compiler, err := closure.ParseGoVersion(command.Go, "--go")
	if err != nil {
		return refuseGopls(1, err.Error())
	}
	if (command.ReviewedOn == "") != (command.Reviewer == "") {
		return refuseGopls(2, "--review-determination and --reviewer go together")
	}
	var reviewedOn *closure.Date
	if command.ReviewedOn != "" {
		date, err := closure.ParseDate(command.ReviewedOn, "--review-determination")
		if err != nil {
			return refuseGopls(1, err.Error())
		}
		reviewedOn = &date
	}
	// The two refusals that cost nothing and must be reported as usage: they are
	// settled before a compiler is resolved or a directory inspected.
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		return refuseGopls(2, "refusing stage in GitHub Actions; pin maintenance is "+
			"maintainer-host work")
	}
	goBinary, err := goplsstage.ResolveGo()
	if err != nil {
		return refuseGopls(1, err.Error())
	}
	proposal, err := goplsstage.Stage(context.Background(), goplsstage.Request{
		Release: release, Compiler: compiler, Current: command.Current, Out: command.Out,
		Dockerfile: command.Dockerfile, ReviewedOn: reviewedOn, Reviewer: command.Reviewer,
		Go: goBinary,
	}, goplsstage.Dependencies{
		Environment:   environment,
		Transport:     seams.transport(),
		GitHubActions: os.Getenv("GITHUB_ACTIONS") == "true",
	})
	if err != nil {
		return refuseGopls(1, err.Error())
	}
	fmt.Println(proposal.Summary)
	return 0
}

// compareProposal binds an approved run to the proposal a human actually reviewed.
//
// The four data files *and* the Dockerfile are compared byte for byte; the two pin
// documents are compared as decoded documents with the review date and reviewer
// cleared, because those are the two fields a renewal is allowed to add. It is
// therefore four byte comparisons, one byte comparison of the recipe and a semantic
// comparison of the record — not six raw byte comparisons, and not a re-validation of
// either document.
func compareProposal(proposalRoot, stagedRoot string) error {
	proposal, staged := filepath.Join(proposalRoot, "gopls-closure"),
		filepath.Join(stagedRoot, "gopls-closure")
	info, err := os.Stat(proposalRoot)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("--proposal is not a directory: %s", proposalRoot)
	}
	if _, err := os.Stat(filepath.Join(proposal, goplsstage.ProposalFile)); err != nil {
		return fmt.Errorf("--proposal does not look like a retained proposal: missing %s",
			goplsstage.ProposalFile)
	}
	for _, name := range []closure.Filename{closure.FileGoMod, closure.FileGoSum,
		closure.FileRootGoMod, closure.FileLicenses} {
		if !sameBytes(filepath.Join(proposal, string(name)),
			filepath.Join(staged, string(name))) {
			return fmt.Errorf("the reviewed proposal changed since it was retained: %s", name)
		}
	}
	proposalDockerfile := filepath.Join(proposalRoot, "Dockerfile")
	if _, err := os.Stat(proposalDockerfile); err != nil {
		return errors.New("--proposal does not look like a retained proposal: missing Dockerfile")
	}
	if !sameBytes(proposalDockerfile, filepath.Join(stagedRoot, "Dockerfile")) {
		return errors.New("the reviewed proposal changed since it was retained: Dockerfile")
	}
	proposalDocument, err := readDocument(filepath.Join(proposal, goplsstage.ProposalFile))
	if err != nil {
		return err
	}
	stagedDocument, err := readDocument(filepath.Join(staged, string(closure.FilePin)))
	if err != nil {
		return err
	}
	for _, document := range []map[string]any{proposalDocument, stagedDocument} {
		authentication, ok := document["authentication"].(map[string]any)
		if !ok {
			return errors.New("the reviewed proposal changed since it was retained: no " +
				"authentication record to compare")
		}
		authentication["reviewed_on"] = nil
		authentication["reviewer"] = nil
	}
	if !reflect.DeepEqual(proposalDocument, stagedDocument) {
		return errors.New("the reviewed proposal changed since it was retained: " +
			differingKeys(proposalDocument, stagedDocument))
	}
	return nil
}

// readDocument decodes one retained record. The proposal is not a readable pin (its
// review fields are null), so it is compared as a document rather than loaded.
func readDocument(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the reviewed proposal changed since it was retained: %v", err)
	}
	var document map[string]any
	if err := decodeDocument(raw, &document); err != nil {
		return nil, fmt.Errorf("the reviewed proposal changed since it was retained: %v", err)
	}
	return document, nil
}

// differingKeys names the members two decoded records disagree on.
func differingKeys(left, right map[string]any) string {
	names := []string{}
	for key := range left {
		if !reflect.DeepEqual(left[key], right[key]) {
			names = append(names, key)
		}
	}
	for key := range right {
		if _, present := left[key]; present {
			continue
		}
		names = append(names, key)
	}
	if len(names) == 0 {
		return "the documents differ"
	}
	joined := names[0]
	for _, name := range names[1:] {
		joined += ", " + name
	}
	return joined
}

// sameBytes compares two files' bytes, treating an unreadable side as a difference.
func sameBytes(left, right string) bool {
	leftRaw, leftErr := os.ReadFile(left)
	rightRaw, rightErr := os.ReadFile(right)
	return leftErr == nil && rightErr == nil && string(leftRaw) == string(rightRaw)
}

// refuseGopls writes one contract line under the verb's frozen prefix.
func refuseGopls(status int, message string) int {
	_, _ = os.Stderr.WriteString(goplsstage.Prefix + ": " + trimNewline(message) + "\n")
	return status
}

func trimNewline(text string) string {
	for len(text) > 0 && text[len(text)-1] == '\n' {
		text = text[:len(text)-1]
	}
	return text
}
