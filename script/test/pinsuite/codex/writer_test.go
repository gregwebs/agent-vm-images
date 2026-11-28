package codex

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// writerFixture is one writer case's template closure, its served tree and its
// reviewed verifier.
type writerFixture struct {
	base     string
	template *fixture
	upstream string
	cosign   string
}

// newWriterFixture builds the three things every writer case needs from one closure:
// the closure itself, the tree `--fixture-upstream` serves and the fixture cosign.
func newWriterFixture(r harness.Reporter, label string) *writerFixture {
	r.Helper()
	base := scratch(r, label)
	built := &writerFixture{base: base}
	built.template = newFixture(r, filepath.Join(base, "template"), Release, nil)
	built.upstream = writeUpstream(r, filepath.Join(base, "upstream"), built.template.served())
	built.cosign = writeCosign(r, filepath.Join(base, "cosign"), cosignRequest{})
	return built
}

// repository is a fresh checkout inside this case: work/repo, with work/tmp as the
// writer's TMPDIR and work/stage as the proposal directory.
func (w *writerFixture) repository(r harness.Reporter, label string) *repository {
	r.Helper()
	return newRepository(r, filepath.Join(w.base, label), Release)
}

// installed is the same checkout with the template closure already committed.
func (w *writerFixture) installed(r harness.Reporter, label string) *repository {
	r.Helper()
	return w.repository(r, label).install(r, w.template)
}

// stageProposal stages one proposal through the real writer, from the served release.
func (w *writerFixture) stageProposal(r harness.Reporter, repo *repository) (string, string) {
	r.Helper()
	stage := repo.stagePath()
	result := runWriter(r, repo, arguments("--version", Release, "--stage-dir", stage,
		"--cosign", w.cosign, "--fixture-upstream", w.upstream))
	requireStatus(r, result, 0)
	return stage, result.Stdout
}

// filesBelow is every file under root, relative and slash-separated, sorted.
func filesBelow(r harness.Reporter, root string) []string {
	r.Helper()
	found := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found = append(found, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		r.Fatalf("cannot walk %s: %v", root, err)
	}
	sort.Strings(found)
	return found
}

// upstreamFiles is the six canonical documents, as `--fixture-upstream` lays them out.
func upstreamFiles() []string {
	urls := []string{apiURL(Release), assetURL(Release, ChecksumsAsset)}
	for _, name := range platforms {
		triple := targetOf(name)
		urls = append(urls, assetURL(Release, assetName(triple)),
			assetURL(Release, bundleName(triple)))
	}
	files := make([]string, 0, len(urls))
	for _, url := range urls {
		_, rest, _ := strings.Cut(url, "://")
		files = append(files, rest)
	}
	sort.Strings(files)
	return files
}

// proposalFiles is the six proposal destinations, relative to the stage directory.
func proposalFiles() []string {
	names := []string{"pin.json", "Dockerfile", "evidence/release.json",
		"evidence/" + ChecksumsAsset}
	for _, name := range platforms {
		names = append(names, "evidence/"+bundleName(targetOf(name)))
	}
	sort.Strings(names)
	return names
}

// requireSameFiles compares two sorted name lists and says what differs.
func requireSameFiles(t *testing.T, got, want []string, what string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s is %v, want %v", what, got, want)
	}
}

// writerRefusalText asserts a refusal's status, prefix and needles and returns the
// message. A case that is also about the sources staying put calls requireRefusal
// instead.
func writerRefusalText(r harness.Reporter, result harness.Result, needles ...string) string {
	r.Helper()
	requireStatus(r, result, 1)
	text, found := strings.CutPrefix(result.Stderr, writerPrefix)
	if !found {
		r.Fatalf("the refusal is not prefixed with %q: %q\n%s", writerPrefix, result.Stderr,
			result.Report())
	}
	text = strings.TrimSpace(text)
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			r.Fatalf("the refusal does not mention %q: %s\n%s", needle, text, result.Report())
		}
	}
	return text
}

// --- --check ------------------------------------------------------------------

// TestWriterCheckAcceptsTheCommittedClosure: `--check` re-verifies the committed
// closure offline and prints the reader's transcript plus the single ARG slot.
func TestWriterCheckAcceptsTheCommittedClosure(t *testing.T) {
	w := newWriterFixture(t, "writer-check")
	repo := w.installed(t, "check-ok")
	result := runWriter(t, repo, arguments("--check"))
	requireStatus(t, result, 0)
	result.RequireStdout(t, fmt.Sprintf("codex %s: offline committed-input check", Release))
	result.RequireStdout(t, dockerfileArg+Release)
}

// TestWriterCheckRefusesADockerfileWithNoCodexARG: the ARG is how a released image
// says which version its binary is, so there must be exactly one.
func TestWriterCheckRefusesADockerfileWithNoCodexARG(t *testing.T) {
	w := newWriterFixture(t, "writer-check-no-slot")
	repo := newRepositoryWithDockerfile(t, filepath.Join(w.base, "no-slot"),
		"FROM scratch\nLABEL org.agent-vm.version.codex=\"x\"\n").install(t, w.template)
	writerRefusalText(t, runWriter(t, repo, arguments("--check")),
		"exactly one `ARG AGENT_VERSION_CODEX=")
}

// TestWriterCheckRefusesASecondCodexARG: one slot, not two.
func TestWriterCheckRefusesASecondCodexARG(t *testing.T) {
	w := newWriterFixture(t, "writer-check-two-slots")
	text := fmt.Sprintf(dockerfileTemplate, Release) + dockerfileArg + Release + "\n"
	repo := newRepositoryWithDockerfile(t, filepath.Join(w.base, "two-slots"), text).
		install(t, w.template)
	writerRefusalText(t, runWriter(t, repo, arguments("--check")),
		"exactly one `ARG AGENT_VERSION_CODEX=")
}

// TestWriterCheckRefusesADockerfileWhoseLabelDoesNotReadTheARG: a static label would
// let the image claim a version its binary is not.
func TestWriterCheckRefusesADockerfileWhoseLabelDoesNotReadTheARG(t *testing.T) {
	w := newWriterFixture(t, "writer-check-no-label")
	text := strings.Replace(fmt.Sprintf(dockerfileTemplate, Release),
		`LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"`,
		`LABEL org.agent-vm.version.codex="static"`, 1)
	repo := newRepositoryWithDockerfile(t, filepath.Join(w.base, "no-label"), text).
		install(t, w.template)
	writerRefusalText(t, runWriter(t, repo, arguments("--check")),
		"must reference ${AGENT_VERSION_CODEX}")
}

// --- the CI refusals ----------------------------------------------------------

// TestWriterTheCIRefusalPrecedesTheFetch: pins are maintainer-host maintenance, so CI
// is refused before a single fetch.
//
// The fixture upstream holds the whole release, so a fetch before the refusal would
// have succeeded and the case would prove nothing. The served tree is emptied to make
// a fetch impossible instead: the refusal must still be the CI one.
func TestWriterTheCIRefusalPrecedesTheFetch(t *testing.T) {
	w := newWriterFixture(t, "writer-ci")
	repo := w.repository(t, "ci")
	before := repo.snapshot(t)
	empty := filepath.Join(w.base, "no-upstream")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("cannot create %s: %v", empty, err)
	}
	result := runWriter(t, repo, arguments("--version", Release, "--cosign", w.cosign,
		"--fixture-upstream", empty), withEnv("GITHUB_ACTIONS", "true"))
	requireRefusal(t, result, "refusing GitHub Actions", repo, before)
}

// TestWriterTheCIRefusalPrecedesPublishing: a staged proposal cannot be published from
// CI either, even though nothing has to be fetched to publish it.
func TestWriterTheCIRefusalPrecedesPublishing(t *testing.T) {
	w := newWriterFixture(t, "writer-publish-ci")
	repo := w.repository(t, "publish-ci")
	stage, _ := w.stageProposal(t, repo)
	before := repo.snapshot(t)
	result := runWriter(t, repo, arguments("--publish-stage", stage),
		withEnv("GITHUB_ACTIONS", "true"))
	requireRefusal(t, result, "refusing GitHub Actions", repo, before)
}

// --- --probe-cosign -----------------------------------------------------------

// TestWriterProbeCosignAcceptsTheReviewedVerifier: the probe reports the exact
// reviewed version and writes nothing.
func TestWriterProbeCosignAcceptsTheReviewedVerifier(t *testing.T) {
	w := newWriterFixture(t, "writer-probe")
	repo := w.installed(t, "probe-ok")
	before := repo.snapshot(t)
	result := runWriter(t, repo, arguments("--probe-cosign", w.cosign))
	requireStatus(t, result, 0)
	want := fmt.Sprintf("cosign %s: %s\n", VerifierVersion, w.cosign)
	if result.Stdout != want {
		t.Fatalf("--probe-cosign printed %q, want %q", result.Stdout, want)
	}
	if after := repo.snapshot(t); !sameSnapshot(after, before) {
		t.Fatalf("the probe wrote to the repository: %s", difference(before, after))
	}
}

// TestWriterProbeCosignRefusesAnotherVersion: the reviewed version is a closed
// constant, so another verifier is refused by name.
func TestWriterProbeCosignRefusesAnotherVersion(t *testing.T) {
	w := newWriterFixture(t, "writer-probe-version")
	other := writeCosign(t, filepath.Join(w.base, "cosign-other"), cosignRequest{version: "v2.2.0"})
	repo := w.installed(t, "probe-version")
	writerRefusalText(t, runWriter(t, repo, arguments("--probe-cosign", other)),
		"reviewed verifier is "+VerifierVersion)
}

// TestWriterProbeCosignRefusesANonExecutable: a file that cannot be executed is not a
// verifier.
func TestWriterProbeCosignRefusesANonExecutable(t *testing.T) {
	w := newWriterFixture(t, "writer-probe-plain")
	plain := filepath.Join(w.base, "cosign-plain")
	harness.WriteFile(t, plain, []byte("not executable\n"))
	repo := w.installed(t, "probe-plain")
	writerRefusalText(t, runWriter(t, repo, arguments("--probe-cosign", plain)),
		"is not an executable file")
}

// --- --stage-dir --------------------------------------------------------------

// TestWriterStageDirWritesTheSixFileProposal: one stage run writes exactly the six
// proposal destinations, leaves the checkout untouched, and the proposal is a closure
// the reader accepts from its own directory.
func TestWriterStageDirWritesTheSixFileProposal(t *testing.T) {
	w := newWriterFixture(t, "writer-stage-ok")
	repo := w.repository(t, "stage-ok")
	before := repo.snapshot(t)
	stage, printed := w.stageProposal(t, repo)
	if !strings.Contains(printed, fmt.Sprintf("%sstaged %s in %s\n", writerPrefix, Release, stage)) {
		t.Fatalf("the writer did not print the frozen staged line: %q", printed)
	}
	requireSameFiles(t, filesBelow(t, stage), proposalFiles(), "the proposal")
	accepts(t, pinInvocation("check", stage, "--release", Release))
	if after := repo.snapshot(t); !sameSnapshot(after, before) {
		t.Fatalf("a staging run wrote into the repository: %s", difference(before, after))
	}
	// The served tree holds exactly the six canonical documents, so a staging run that
	// succeeded read nothing else: the request set is the whole tree.
	requireSameFiles(t, filesBelow(t, w.upstream), upstreamFiles(), "the served tree")
}

// TestWriterStageDirKeepsTheCallersFiles: a directory with anything in it is the
// caller's, and a refused run erases nothing.
func TestWriterStageDirKeepsTheCallersFiles(t *testing.T) {
	w := newWriterFixture(t, "writer-stage-owned")
	repo := w.repository(t, "stage-owned")
	stage := repo.stagePath()
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatalf("cannot create %s: %v", stage, err)
	}
	sentinel := filepath.Join(stage, "mine.txt")
	harness.WriteFile(t, sentinel, []byte("mine\n"))
	before := repo.snapshot(t)
	result := runWriter(t, repo, arguments("--version", Release, "--stage-dir", stage,
		"--cosign", w.cosign, "--fixture-upstream", w.upstream))
	requireRefusal(t, result, "is not empty", repo, before)
	if body := string(harness.ReadFile(t, sentinel)); body != "mine\n" {
		t.Fatalf("the refused run erased a caller file: %q", body)
	}
}

// TestWriterStageDirPrintsUnchangedForAByteIdenticalRepeat: re-staging the committed
// release, from byte-identical served bytes, prints the frozen unchanged line.
//
// The committed closure was verified on a date that has already passed, so the repeat
// is byte-identical only if the writer retains that date: a writer that restamped the
// proposal with today's date would print `staged`, not `unchanged`.
func TestWriterStageDirPrintsUnchangedForAByteIdenticalRepeat(t *testing.T) {
	w := newWriterFixture(t, "writer-stage-repeat")
	repo := w.installed(t, "stage-repeat")
	stage := repo.stagePath()
	committed := document(t, harness.ReadFile(t, filepath.Join(repo.root, PinDirectory,
		"pin.json")))
	verifiedOn := committed["authentication"].(map[string]any)["verified_on"].(string)
	if verifiedOn != fixtureVerifiedOn {
		t.Fatalf("the installed closure says it was verified on %q, not %q", verifiedOn,
			fixtureVerifiedOn)
	}
	if verifiedOn == utcToday().Format(time.DateOnly) {
		t.Fatalf("the installed closure was verified today (%s): a writer that restamped "+
			"the repeat would still be byte-identical, so this case would assert nothing",
			verifiedOn)
	}
	result := runWriter(t, repo, arguments("--version", Release, "--stage-dir", stage,
		"--cosign", w.cosign, "--fixture-upstream", w.upstream))
	requireStatus(t, result, 0)
	want := fmt.Sprintf("%sunchanged (%s); the staged proposal is byte-identical to the "+
		"committed files (%s)\n", writerPrefix, Release, stage)
	if !strings.Contains(result.Stdout, want) {
		t.Fatalf("the repeat did not print the frozen unchanged line\n%s", result.Report())
	}
}

// --- --publish-stage ----------------------------------------------------------

// TestWriterPublishStagePublishesTheProposal: publishing is the staged bytes, as one
// transaction, and the result is a closure the reader accepts.
func TestWriterPublishStagePublishesTheProposal(t *testing.T) {
	w := newWriterFixture(t, "writer-publish")
	repo := w.repository(t, "publish")
	stage, _ := w.stageProposal(t, repo)
	proposal := map[string][]byte{}
	for _, name := range filesBelow(t, stage) {
		proposal[name] = harness.ReadFile(t, filepath.Join(stage, filepath.FromSlash(name)))
	}
	before := repo.snapshot(t)
	result := runWriter(t, repo, arguments("--publish-stage", stage))
	requireStatus(t, result, 0)
	result.RequireStderr(t, "publishing the staged proposal in "+stage)
	result.RequireStderr(t, "owner scratch: "+stage)
	published := repo.committed(t)
	if !bytes.Equal(published[PinDirectory+"/pin.json"], proposal["pin.json"]) {
		t.Fatal("the published pin is not the staged pin")
	}
	if !bytes.Equal(published[Dockerfile], proposal["Dockerfile"]) {
		t.Fatal("the published Dockerfile is not the staged Dockerfile")
	}
	for _, name := range platforms {
		relative := "evidence/" + bundleName(targetOf(name))
		if !bytes.Equal(published[PinDirectory+"/"+relative], proposal[relative]) {
			t.Fatalf("the published %s is not the staged one", relative)
		}
	}
	if after := repo.snapshot(t); sameSnapshot(after, before) {
		t.Fatal("the publication changed nothing")
	}
	accepts(t, pinInvocation("check", repo.pinDirectory(), "--release", Release))
}

// TestWriterAPublishLeavesTheStagesOtherFilesAlone: the stage directory belongs to the
// maintainer who gave it. Publishing from it must not remove it or anything in it, and
// `readStage` tolerates entries it does not own — so a note beside the proposal is the
// witness that the publisher was never handed the caller's directory as scratch.
func TestWriterAPublishLeavesTheStagesOtherFilesAlone(t *testing.T) {
	w := newWriterFixture(t, "writer-publish-keep")
	repo := w.repository(t, "publish-keep")
	stage, _ := w.stageProposal(t, repo)
	note := filepath.Join(stage, "maintainer-notes")
	harness.WriteFile(t, note, []byte("reviewed by hand\n"))
	result := runWriter(t, repo, arguments("--publish-stage", stage))
	requireStatus(t, result, 0)
	if _, err := os.Stat(stage); err != nil {
		t.Fatalf("a successful publish removed the caller's stage directory: %v", err)
	}
	if string(harness.ReadFile(t, note)) != "reviewed by hand\n" {
		t.Fatal("a successful publish removed an unrelated file from the caller's stage")
	}
	accepts(t, pinInvocation("check", repo.pinDirectory(), "--release", Release))
}

// TestWriterPublishStageRefusesAnUnrelatedDockerfileEdit: the proposal may carry the
// Codex ARG and its own single-slot defaults, and nothing else.
func TestWriterPublishStageRefusesAnUnrelatedDockerfileEdit(t *testing.T) {
	w := newWriterFixture(t, "writer-publish-arg")
	repo := w.repository(t, "publish-arg")
	stage, _ := w.stageProposal(t, repo)
	dockerfile := filepath.Join(stage, "Dockerfile")
	text := string(harness.ReadFile(t, dockerfile))
	const marker = "ARG AGENT_VERSION_CLAUDE=2.1.286"
	if strings.Count(text, marker) != 1 {
		t.Fatalf("the staged Dockerfile does not carry one Claude ARG: %q", text)
	}
	harness.WriteFile(t, dockerfile, []byte(strings.Replace(text, marker,
		"ARG AGENT_VERSION_CLAUDE=9.9.9", 1)))
	before := repo.snapshot(t)
	result := runWriter(t, repo, arguments("--publish-stage", stage))
	requireRefusal(t, result, "changes more than the single-slot version defaults", repo, before)
}

// --- the identity refusals ----------------------------------------------------

// TestWriterASameReleaseRewriteIsRefused: a release whose served bytes changed under
// the same version is a separate reviewed adoption, never a silent re-pin.
func TestWriterASameReleaseRewriteIsRefused(t *testing.T) {
	w := newWriterFixture(t, "writer-rewrite")
	repo := w.installed(t, "rewrite")
	members := defaultMembers(t, w.template.version, linuxAMD64, machineOf(targetAMD64))
	members = append(members,
		harness.Member{Name: "codex-resources/voice", Typeflag: tar.TypeDir, Mode: 0o755},
		harness.Member{Name: "codex-resources/voice/note.txt", Body: []byte("rewritten\n"), Mode: 0o644})
	rewritten := buildArchive(t, archiveRequest{version: w.template.version,
		platform: linuxAMD64, members: members})
	local := newFixture(t, filepath.Join(scratch(t, "rewrite-source"), "source"), Release,
		map[platform][]byte{linuxAMD64: rewritten, linuxARM64: w.template.archives[linuxARM64]})
	served := writeUpstream(t, filepath.Join(scratch(t, "rewrite-upstream"), "upstream"), local.served())
	before := repo.snapshot(t)
	result := runWriter(t, repo, arguments("--version", Release, "--stage-dir", repo.stagePath(),
		"--cosign", w.cosign, "--fixture-upstream", served))
	requireRefusal(t, result, "changed for the already-pinned", repo, before)
}

// TestWriterARefusingVerifierStagesNothing: the verifier is the only thing that can
// reach the "cosign refused" refusal, and a refusal leaves no proposal behind.
func TestWriterARefusingVerifierStagesNothing(t *testing.T) {
	w := newWriterFixture(t, "writer-refused")
	refusing := writeCosign(t, filepath.Join(w.base, "cosign-refusing"), cosignRequest{verify: 1})
	repo := w.repository(t, "refused")
	stage := repo.stagePath()
	before := repo.snapshot(t)
	result := runWriter(t, repo, arguments("--version", Release, "--stage-dir", stage,
		"--cosign", refusing, "--fixture-upstream", w.upstream))
	requireRefusal(t, result, "cosign refused the committed bundle", repo, before)
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("a refusing verifier left a staging directory at %s", stage)
	}
}

// --- the verifier's contract --------------------------------------------------

// TestWriterTheVerifierGetsTheReviewedArgvAndEnvironment: the reviewed argv, and the
// environment the writer constructs for it.
//
// The child is the only observer of both: production builds that environment rather
// than inheriting it, which is what keeps an ambient `SIGSTORE_*`, a token or a proxy
// from changing what "verified" means.
func TestWriterTheVerifierGetsTheReviewedArgvAndEnvironment(t *testing.T) {
	w := newWriterFixture(t, "writer-verifier")
	record := filepath.Join(w.base, "cosign-record")
	recording := writeCosign(t, filepath.Join(w.base, "cosign-recording"),
		cosignRequest{record: record})
	repo := w.repository(t, "verifier")
	stage := repo.stagePath()
	result := runWriter(t, repo, arguments("--version", Release, "--stage-dir", stage,
		"--cosign", recording, "--fixture-upstream", w.upstream))
	requireStatus(t, result, 0)
	runs := readCosignRecord(t, record)
	if len(runs) != len(platforms) {
		t.Fatalf("the verifier ran %d times, want %d", len(runs), len(platforms))
	}
	wantEnv := []string{"HOME", "LC_ALL", "PATH", "XDG_CACHE_HOME"}
	for _, run := range runs {
		argv := run.argv
		if len(argv) != 8 {
			t.Fatalf("the verifier got %d arguments: %v", len(argv), argv)
		}
		if argv[0] != "verify-blob" {
			t.Fatalf("the verifier was not asked to verify a blob: %v", argv)
		}
		if argv[1] != "--bundle" {
			t.Fatalf("no bundle: %v", argv)
		}
		if argv[3] != "--certificate-identity" {
			t.Fatalf("no identity: %v", argv)
		}
		if argv[5] != "--certificate-oidc-issuer" {
			t.Fatalf("no issuer: %v", argv)
		}
		if argv[4] != identity(Release) {
			t.Fatalf("the identity is not the reviewed one: %s", argv[4])
		}
		if argv[6] != Issuer {
			t.Fatalf("the issuer is not the reviewed one: %s", argv[6])
		}
		if !strings.HasSuffix(argv[2], ".sigstore") {
			t.Fatalf("the bundle is not a .sigstore file: %s", argv[2])
		}
		if !strings.HasSuffix(argv[7], ".tar.gz") {
			t.Fatalf("the payload is not the package archive: %s", argv[7])
		}
		// `PWD`, `OLDPWD`, `SHLVL` and `_` are the verifying shell's own, not inherited
		// configuration: everything else has to be the four entries production
		// constructed, so no ambient trust setting survives.
		inherited := []string{}
		for _, name := range run.env {
			if !contains(shellLocals, name) {
				inherited = append(inherited, name)
			}
		}
		if strings.Join(inherited, " ") != strings.Join(wantEnv, " ") {
			t.Fatalf("the verifier inherited more than the constructed environment: %v",
				run.env)
		}
	}
}

// --- the clock ----------------------------------------------------------------

// clockZones are the two extreme zones, which differ from UTC in opposite directions:
// Kiritimati is UTC+14 and Etc/GMT+12 is UTC-12, so between them every hour of the UTC
// day is covered by a host whose local date differs, which is what makes the pair
// time-of-day independent instead of a coin flip.
var clockZones = []string{"Pacific/Kiritimati", "Etc/GMT+12"}

// atAStableUTCDate runs call unless the UTC date rolls over mid-case, then retries
// once.
//
// The case derives a pin date from "the UTC date right now". A case that straddles UTC
// midnight would compare against a date the process no longer agrees with, so a
// genuine rollover is retried rather than reported -- but only when the date really
// moved, never to excuse a mutant. call reports its behavioural assertions as errors
// rather than stopping the test, which is what makes the retry possible; a fixture
// defect still stops the case.
func atAStableUTCDate(t *testing.T, what string, call func(t *testing.T, today time.Time) error) {
	t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		today := utcToday()
		err := call(t, today)
		if utcToday().Equal(today) {
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			return
		}
	}
	t.Fatalf("%s: the UTC date rolled over twice while this case ran", what)
}

// TestWriterThePinDateIsTheUTCDateInEitherExtremeZone: the pin date is the UTC date,
// whatever the host's local date is.
//
// A writer that stamped its own local calendar is caught here in whichever zone
// differs, and the case proves that at least one of the two does -- otherwise the run
// would be a coin flip that could pass without testing anything.
func TestWriterThePinDateIsTheUTCDateInEitherExtremeZone(t *testing.T) {
	differs := false
	for _, zone := range clockZones {
		location, err := time.LoadLocation(zone)
		if err != nil {
			// A failure, not a skip: the case runs a real process under a real zone,
			// and a host without one would report green while asserting nothing. Go
			// falls back to its own zoneinfo, so this only fires on a broken install.
			t.Fatalf("the zone database is required for this case's teeth: %v", err)
		}
		if time.Now().In(location).Format(time.DateOnly) != utcToday().Format(time.DateOnly) {
			differs = true
		}
	}
	if !differs {
		t.Fatalf("neither extreme zone's local date differs from the UTC date, so a " +
			"local-date stamp could pass this case")
	}
	atAStableUTCDate(t, "either extreme zone", func(t *testing.T, today time.Time) error {
		for _, zone := range clockZones {
			w := newWriterFixture(t, "clock-"+fileName(zone))
			repo := w.repository(t, "clock")
			stage := repo.stagePath()
			result := runWriter(t, repo, arguments("--version", Release, "--stage-dir", stage,
				"--cosign", w.cosign, "--fixture-upstream", w.upstream), withEnv("TZ", zone))
			if result.Status != 0 {
				return fmt.Errorf("staging under TZ=%s exited %d\n%s", zone, result.Status,
					result.Report())
			}
			stamped, ok := object(t, document(t, harness.ReadFile(t,
				filepath.Join(stage, "pin.json"))), "authentication")["verified_on"].(string)
			if !ok {
				return fmt.Errorf("under TZ=%s the writer stamped no verified_on date", zone)
			}
			if stamped != today.Format(time.DateOnly) {
				return fmt.Errorf("under TZ=%s the writer stamped %s, not the UTC date %s",
					zone, stamped, today.Format(time.DateOnly))
			}
			// What it stamped has to verify from this closure's own directory.
			if checked := runReader(t, pinInvocation("check", stage, "--release", Release),
				nil); checked.Status != 0 {
				return fmt.Errorf("the closure staged under TZ=%s was refused\n%s", zone,
					checked.Report())
			}
		}
		return nil
	})
}

// --- the malformed invocations ------------------------------------------------

// TestWriterAMalformedInvocationExitsTwoWithTheWriterPrefix: each mode takes only the
// flags it names, and a malformed invocation is a usage error, not a refusal.
func TestWriterAMalformedInvocationExitsTwoWithTheWriterPrefix(t *testing.T) {
	w := newWriterFixture(t, "writer-usage")
	repo := w.installed(t, "usage")
	for _, testCase := range []struct {
		argv   []string
		needle string
	}{
		{[]string{"--check", "--version", Release}, "--check takes no other argument"},
		{[]string{"--check", "--cosign", w.cosign}, "--check takes no other argument"},
		{[]string{"--probe-cosign", w.cosign, "--version", Release},
			"--probe-cosign takes only --root"},
		{[]string{"--publish-stage", repo.stagePath(), "--version", Release},
			"--publish-stage takes only --root"},
		{nil, "--version is required for a write run"},
	} {
		result := runWriter(t, repo, arguments(testCase.argv...))
		requireStatus(t, result, 2)
		text, found := strings.CutPrefix(result.Stderr, writerPrefix)
		if !found {
			t.Fatalf("%v is not prefixed with %q: %q", testCase.argv, writerPrefix, result.Stderr)
		}
		if !strings.Contains(text, testCase.needle) {
			t.Fatalf("%v did not mention %q: %s", testCase.argv, testCase.needle, text)
		}
	}
}

// TestWriterAReleasedTagThatIsNotExactIsRefused: `latest`, `rust-v0.159` and anything
// else that is not an exact tag cannot name bytes, so the writer refuses it.
func TestWriterAReleasedTagThatIsNotExactIsRefused(t *testing.T) {
	w := newWriterFixture(t, "writer-release")
	repo := w.installed(t, "release")
	result := runWriter(t, repo, arguments("--version", "rust-v0.159", "--stage-dir",
		repo.stagePath(), "--cosign", w.cosign, "--fixture-upstream", w.upstream))
	writerRefusalText(t, result, "not an exact Codex version")
}
