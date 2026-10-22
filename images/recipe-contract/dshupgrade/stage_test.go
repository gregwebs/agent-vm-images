package dshupgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

const fixtureRoot = "../../../script/test/fixtures/pnpm-lock"

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	destination := t.TempDir()
	// macOS TMPDIR is a symlink; resolving it keeps the proposal's realpath
	// comparison deterministic across platforms.
	if resolved, err := filepath.EvalSymlinks(destination); err == nil {
		destination = resolved
	}
	source := filepath.Join(fixtureRoot, name)
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return destination
}

func TestStageTentative(t *testing.T) {
	before := copyFixture(t, "update-before")
	stage := filepath.Join(t.TempDir(), "stage")
	if err := StageTentative(StageRequest{Before: pnpmlock.Prefix(before), Stage: stage, Argument: "0.1.5-rc.1"}); err != nil {
		t.Fatal(err)
	}
	manifest, err := document.ReadFile(filepath.Join(stage, "package.json"), document.JSON)
	if err != nil {
		t.Fatal(err)
	}
	dependency := manifest.Get("dependencies").Get("@deepseek-ai/dsh")
	if text, ok := dependency.Text(); !ok || text.Display() != "0.1.5-rc.1" {
		t.Errorf("dependency = %s", dependency.Describe())
	}
	workspace, err := document.ReadFile(filepath.Join(stage, "pnpm-workspace.yaml"), document.YAML)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Has("overrides") {
		t.Error("the tentative workspace still carries overrides")
	}
	if !workspace.Has("packages") {
		t.Error("the tentative workspace lost unrelated keys")
	}
	// Every other manifest key survives the rewrite.
	if !manifest.Has("private") || !manifest.Has("packageManager") {
		t.Error("the tentative manifest lost keys")
	}
	if err := StageTentative(StageRequest{Before: pnpmlock.Prefix(before), Stage: stage, Argument: "latest"}); err == nil || !strings.Contains(err.Error(), "expected target exact selection") {
		t.Errorf("invalid target = %v", err)
	}
	if err := StageTentative(StageRequest{Before: pnpmlock.Prefix(before), Stage: filepath.Join(t.TempDir(), "again"), Argument: "0.1.5-rc.1", ExtraSet: true}); err == nil {
		t.Error("a tentative stage accepted an extra argument")
	}
}

func TestStageFinalRequiresReviewedMap(t *testing.T) {
	before := copyFixture(t, "update-before")
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "tentative"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(before, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "tentative", "package.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	proofPath := filepath.Join(work, "proof.json")
	proof := `{"overrides":{"@deepseek-ai/dsh>@deepseek-ai/cordis":"4.0.3",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader":"1.0.3",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer":"1.1.4",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include":"1.0.7"},"requirements":[]}`
	if err := os.WriteFile(proofPath, []byte(proof), 0o644); err != nil {
		t.Fatal(err)
	}
	err = StageFinal(StageRequest{
		Before: pnpmlock.Prefix(before), Stage: filepath.Join(work, "unreviewed"), Argument: proofPath,
	})
	if err == nil || !strings.Contains(err.Error(), "Cordis map changed") {
		t.Errorf("changed map without review = %v", err)
	}
	// With an explicit review the same map is written.
	approved := filepath.Join(work, "approved.json")
	if err := os.WriteFile(approved, []byte(`{"@deepseek-ai/dsh>@deepseek-ai/cordis":"4.0.3",`+
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader":"1.0.3",`+
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer":"1.1.4",`+
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include":"1.0.7"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageFinal(StageRequest{
		Before: pnpmlock.Prefix(before), Stage: filepath.Join(work, "final"), Argument: proofPath,
		Extra: approved, ExtraSet: true,
	}); err != nil {
		t.Fatal(err)
	}
	workspace, err := document.ReadFile(filepath.Join(work, "final", "pnpm-workspace.yaml"), document.YAML)
	if err != nil {
		t.Fatal(err)
	}
	cordis := workspace.Get("overrides").Get("@deepseek-ai/dsh>@deepseek-ai/cordis")
	if text, ok := cordis.Text(); !ok || text.Display() != "4.0.3" {
		t.Errorf("overrides = %s", workspace.Get("overrides").Describe())
	}
	if _, err := os.Stat(filepath.Join(work, "final", "pnpm-lock.yaml")); err != nil {
		t.Errorf("the final stage did not copy the lock: %v", err)
	}
}

func TestProposalPublication(t *testing.T) {
	prefix := copyFixture(t, "derivation")
	work := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	request := DeriveRequest{
		Tentative: pnpmlock.Prefix(prefix),
		Propose:   filepath.Join(work, "proposal.json"),
		Cwd:       cwd,
	}
	if _, err := Derive(request); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(request.Propose)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(payload), "}\n") || !strings.Contains(string(payload), "\n  \"") {
		t.Errorf("proposal is not the indented map with a final newline:\n%s", payload)
	}
	if entries, err := document.Parse(payload, document.JSON); err != nil || len(entries.Keys()) != 4 {
		t.Errorf("proposal = %v, %v", err, entries)
	}
	// link never replaces an existing file.
	if _, err := Derive(request); err == nil || !strings.Contains(err.Error(), "file already exists") {
		t.Errorf("second publication = %v", err)
	}
	// A collision with a project input is refused.
	collision := DeriveRequest{
		Tentative: pnpmlock.Prefix(prefix),
		Propose:   filepath.Join(prefix, "package.json"),
		Cwd:       cwd,
	}
	if _, err := Derive(collision); err == nil || !strings.Contains(err.Error(), "proposal collides with input") {
		t.Errorf("collision = %v", err)
	}
	// A relative path resolves against the physical working directory.
	relative := DeriveRequest{
		Tentative: pnpmlock.Prefix(prefix),
		Propose:   "relative-proposal.json",
		Cwd:       work,
	}
	if _, err := Derive(relative); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "relative-proposal.json")); err != nil {
		t.Errorf("relative proposal = %v", err)
	}
}

func TestDeriveReviewedMap(t *testing.T) {
	prefix := copyFixture(t, "derivation")
	work := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	overrides := `{"@deepseek-ai/dsh>@deepseek-ai/cordis":"4.0.2",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader":"1.0.3",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer":"1.1.4",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include":"1.0.7"}`
	approved := filepath.Join(work, "approved.json")
	if err := os.WriteFile(approved, []byte(overrides), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Derive(DeriveRequest{Tentative: pnpmlock.Prefix(prefix), Approved: approved, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Requirements) == 0 {
		t.Error("no requirements")
	}
	wrong := filepath.Join(work, "wrong.json")
	changed := strings.Replace(overrides, `@deepseek-ai/cordis":"4.0.2"`, `@deepseek-ai/cordis":"4.0.3"`, 1)
	if changed == overrides {
		t.Fatal("the wrong-review edit did not apply")
	}
	if err := os.WriteFile(wrong, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Derive(DeriveRequest{Tentative: pnpmlock.Prefix(prefix), Approved: wrong, Cwd: cwd}); err == nil ||
		!strings.Contains(err.Error(), "reviewed map differs from independent tentative requirements") {
		t.Errorf("wrong review = %v", err)
	}
}
