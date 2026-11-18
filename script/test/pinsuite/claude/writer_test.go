package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// nextVersion is the release a bump case publishes: one the fixture's committed
// closure has never seen.
const nextVersion = "9.9.10"

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

// platformDigest is one executable's SHA-256, as the pin records it.
func platformDigest(r harness.Reporter, path string) string {
	r.Helper()
	return harness.SHA256Hex(harness.ReadFile(r, path))
}

// TestWriterFirstAdoptionPublishesTheFourFilesAndANoneDelta: a clean first adoption
// publishes the pin, both evidence files and the ARG, reports the delta from nothing,
// and a repeat run is byte-identical.
//
// This is the one writer case that drives `script/dev/pin-claude.sh` itself: the
// compile, the `cd` to the checkout and the `exec avm-pin claude` CONTRIBUTING tells a
// maintainer to run. The other cases drive the binary the wrapper builds.
func TestWriterFirstAdoptionPublishesTheFourFilesAndANoneDelta(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-first"), unadopted(), dockerfileAt("0.0.0"))
	result := repo.runWrapper(t, flags("--version", Version))
	result.RequireStatus(t, 0)
	result.RequireStdout(t, "claude payload delta: (none) -> "+Version)

	want := []string{
		dockerfile,
		evidenceDirectory + "/" + manifestName,
		evidenceDirectory + "/" + signatureName,
		pinFile,
	}
	sort.Strings(want)
	if published := publishedFiles(result.Stdout); strings.Join(published, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the published files are %v, want %v", published, want)
	}

	pin := repo.pin(t)
	if pin["version"] != Version {
		t.Fatalf("the pin names %v, want %s", pin["version"], Version)
	}
	signer := testFixture(t).signer.Fingerprint
	if object(t, pin, "key")["fingerprint"] != string(signer) {
		t.Fatalf("the pin names %v, not the fixture signer %s",
			object(t, pin, "key")["fingerprint"], signer)
	}
	for _, name := range platforms {
		pinned := object(t, pin, "platforms", name.key)
		served := repo.executables[name.token]
		if fmt.Sprint(pinned["sha256"]) != platformDigest(t, served) {
			t.Fatalf("%s digest %v is not the served executable", name.key, pinned["sha256"])
		}
		if fmt.Sprint(pinned["size"]) != strconv.Itoa(len(harness.ReadFile(t, served))) {
			t.Fatalf("%s size %v is not the served executable", name.key, pinned["size"])
		}
		url, ok := pinned["url"].(string)
		if !ok || !strings.HasSuffix(url, "/"+Version+"/"+string(name.token)+"/claude") {
			t.Fatalf("%s URL %v is not canonical", name.key, pinned["url"])
		}
	}
	text := repo.dockerfile(t)
	if !strings.Contains(text, dockerfileArg+Version) {
		t.Fatal("the published Dockerfile does not carry the new ARG")
	}
	if strings.Contains(text, dockerfileArg+"0.0.0") {
		t.Fatal("the published Dockerfile still carries the old ARG")
	}

	before := repo.snapshot(t)
	again := repo.runWrapper(t, flags("--version", Version))
	again.RequireStatus(t, 0)
	again.RequireStdout(t, "unchanged")
	if after := repo.snapshot(t); !sameSnapshot(after, before) {
		t.Fatalf("a repeat run changed a committed byte: %s", after.difference(before))
	}
}

// TestWriterBumpPublishesAndTheDeltaListsBothPlatforms: a new release is published, and
// the delta shows each platform's old and new digest.
func TestWriterBumpPublishesAndTheDeltaListsBothPlatforms(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-bump"))
	fresh := newProposal(t, filepath.Join(repo.base, "new"), nextVersion, testFixture(t).signer)
	old := map[token]string{}
	for _, name := range platforms {
		old[name.token] = platformDigest(t, repo.executables[name.token])
	}
	repo.serve(t, served{closure: fresh.closure, executables: fresh.executables, version: nextVersion})

	result := repo.run(t, flags("--version", nextVersion))
	result.RequireStatus(t, 0)
	result.RequireStdout(t, "claude payload delta: "+Version+" -> "+nextVersion)
	for _, name := range platforms {
		updated := platformDigest(t, fresh.executables[name.token])
		if !strings.Contains(result.Stdout, old[name.token]) ||
			!strings.Contains(result.Stdout, updated) {
			t.Fatalf("the delta does not show %s old and new digests\n%s", name.key,
				result.Report())
		}
	}
	pin := repo.pin(t)
	if pin["version"] != nextVersion {
		t.Fatalf("the pin names %v, want %s", pin["version"], nextVersion)
	}
	if !strings.Contains(repo.dockerfile(t), dockerfileArg+nextVersion) {
		t.Fatal("the published Dockerfile does not carry the new ARG")
	}
}

// TestWriterSameReleaseRewriteRefusedWithSourcesUnchanged: a release whose served bytes
// changed under the same version is a separate reviewed adoption, never a silent
// re-pin -- and the refusal leaves every source byte where it was.
func TestWriterSameReleaseRewriteRefusedWithSourcesUnchanged(t *testing.T) {
	fixture := testFixture(t)
	rows := []struct {
		name   string
		mutate func(t *testing.T, repo *writerRepo, base string)
		needle string
	}{
		{
			name: "manifest",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				mutant := filepath.Join(base, "mutant")
				harness.CopyTree(t, repo.closure, mutant)
				rewriteManifest(t, mutant, fixture.signer, func(manifest map[string]any) {
					manifest["note"] = "upstream re-rendered"
				})
				repo.serve(t, repo.serving(mutant, repo.executables))
			},
			needle: "the signed manifest changed for the already-pinned",
		},
		{
			name: "signature",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				mutant := filepath.Join(base, "mutant")
				harness.CopyTree(t, repo.closure, mutant)
				// A different creation time is a different valid signature over the same
				// bytes: EdDSA is deterministic, so re-signing now would reproduce the
				// first one.
				signature := sign(t, fixture.signer,
					filepath.Join(mutant, "evidence", manifestName), time.Now().Unix()+3600)
				harness.WriteFile(t, filepath.Join(mutant, "evidence", signatureName), signature)
				rewritePin(t, mutant, func(pin map[string]any) {
					object(t, pin, "signature")["sha256"] = harness.SHA256Hex(signature)
				})
				repo.serve(t, repo.serving(mutant, repo.executables))
			},
			needle: "the detached signature changed for the already-pinned",
		},
		{
			name: "executable",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				replaced := filepath.Join(base, "replaced-x64")
				harness.WriteFile(t, replaced,
					append(harness.ReadFile(t, repo.executables[linuxX64]), 0))
				repo.serve(t, repo.serving(repo.closure, map[token]string{
					linuxX64:   replaced,
					linuxARM64: repo.executables[linuxARM64],
				}))
			},
			needle: "but the signed manifest records",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			repo := newWriterRepo(t, scratch(t, "writer-same-release-"+row.name))
			row.mutate(t, repo, scratch(t, "writer-same-release-mutant-"+row.name))
			before := repo.snapshot(t)
			requireRefusal(t, repo.run(t, flags("--version", Version)), row.needle, repo, before)
		})
	}
}

// TestWriterUpstreamAndSignerFailuresRefusedWithSourcesUnchanged: every way the served
// tree or the signer can disagree is refused, and a refused run changes no byte.
func TestWriterUpstreamAndSignerFailuresRefusedWithSourcesUnchanged(t *testing.T) {
	fixture := testFixture(t)
	rows := []struct {
		name   string
		mutate func(t *testing.T, repo *writerRepo, base string)
		needle string
	}{
		{
			name: "published key",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				attacker := makeSigner(t, filepath.Join(base, "attacker"), false)
				harness.CopyFile(t, attacker.Key, repo.published(upstreamKeyURL))
			},
			needle: "the published release key changed",
		},
		{
			name: "signature by another key",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				attacker := makeSigner(t, filepath.Join(base, "attacker"), false)
				signature := sign(t, attacker, filepath.Join(repo.closure, "evidence", manifestName), 0)
				harness.WriteFile(t, repo.published(upstreamBase+"/"+Version+"/"+signatureName),
					signature)
			},
			needle: "signature does not verify",
		},
		{
			name: "manifest version",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				proposal := newProposal(t, filepath.Join(base, "proposal"), nextVersion, fixture.signer)
				repo.serve(t, served{closure: proposal.closure, executables: proposal.executables,
					version: Version})
			},
			needle: "signed manifest names " + nextVersion,
		},
		{
			name: "size only",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				mutant := filepath.Join(base, "mutant")
				harness.CopyTree(t, repo.closure, mutant)
				rewriteManifest(t, mutant, fixture.signer, func(manifest map[string]any) {
					pinned := object(t, manifest, "platforms", string(linuxX64))
					size, err := strconv.Atoi(fmt.Sprint(pinned["size"]))
					if err != nil {
						t.Fatalf("the manifest size is not a number: %v", err)
					}
					pinned["size"] = size + 1
				})
				repo.serve(t, repo.serving(mutant, repo.executables))
			},
			needle: "but the signed manifest records",
		},
		{
			name: "manifest missing platform",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				mutant := filepath.Join(base, "mutant")
				harness.CopyTree(t, repo.closure, mutant)
				rewriteManifest(t, mutant, fixture.signer, func(manifest map[string]any) {
					delete(object(t, manifest, "platforms"), string(linuxX64))
				})
				repo.serve(t, repo.serving(mutant, repo.executables))
			},
			needle: "has no linux-x64 platform",
		},
		{
			name: "wrong ELF machine",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				arm64 := elfBlob(t, filepath.Join(base, "arm64"), arm64Machine, elfShape{})
				closure := buildClosure(t, closureRequest{
					Out: filepath.Join(base, "closure"), Signer: fixture.signer,
					Version: Version, AMD64: arm64, ARM64: arm64,
				})
				repo.serve(t, served{closure: closure, executables: map[token]string{
					linuxX64: arm64, linuxARM64: arm64,
				}})
			},
			needle: "targets ELF machine 183, not 62",
		},
		{
			name: "truncated executable",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				short := filepath.Join(base, "short")
				harness.WriteFile(t, short, harness.ReadFile(t, repo.executables[linuxX64])[:100])
				repo.serve(t, repo.serving(repo.closure, map[token]string{
					linuxX64:   short,
					linuxARM64: repo.executables[linuxARM64],
				}))
			},
			needle: "but the signed manifest records",
		},
		{
			name: "missing executable",
			mutate: func(t *testing.T, repo *writerRepo, base string) {
				remove(t, repo.published(upstreamBase+"/"+Version+"/"+string(linuxARM64)+"/claude"))
			},
			needle: "/" + Version + "/linux-arm64/claude",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			repo := newWriterRepo(t, scratch(t, "writer-failure-"+row.name))
			row.mutate(t, repo, scratch(t, "writer-failure-mutant-"+row.name))
			before := repo.snapshot(t)
			requireRefusal(t, repo.run(t, flags("--version", Version)), row.needle, repo, before)
		})
	}
}

// TestWriterClosureUnderAnUnreviewedSignerRefused: without the suite's `--signer`, a
// fixture closure is signed by a primary the review has never seen.
func TestWriterClosureUnderAnUnreviewedSignerRefused(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-unreviewed"), unsigned())
	before := repo.snapshot(t)
	requireRefusal(t, repo.run(t, flags("--version", Version)), "reviewed primary", repo, before)
}

// TestWriterGitHubActionsRefusedBeforeAnyFetchCheckAllowed: pins are maintainer-host
// maintenance, so CI is refused before the first fetch -- and `--check` still reads.
func TestWriterGitHubActionsRefusedBeforeAnyFetchCheckAllowed(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-ci"))
	before := repo.snapshot(t)
	requireRefusal(t, repo.run(t, flags("--version", Version), withEnv("GITHUB_ACTIONS", "true")),
		"refusing GitHub Actions", repo, before)
	allowed := repo.run(t, flags("--check"), withEnv("GITHUB_ACTIONS", "true"))
	allowed.RequireStatus(t, 0)
	allowed.RequireStdout(t, "offline committed-input check")
}

// TestWriterEveryRequestIsACanonicalURL: every fetch is one of the five canonical URLs
// on downloads.claude.ai.
//
// The served tree holds exactly those five files, so a run that reaches any other URL
// has nothing to read and refuses; a lookalike host that *would* serve a request is
// placed with different bytes, so reading it would be caught by the digests. What the
// run placed is therefore the canonical bytes from the canonical origin, whatever order
// it asked in.
func TestWriterEveryRequestIsACanonicalURL(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-fetches"))
	expected := []string{
		upstreamBase + "/" + Version + "/" + manifestName,
		upstreamBase + "/" + Version + "/" + signatureName,
		upstreamKeyURL,
		upstreamBase + "/" + Version + "/" + string(linuxX64) + "/claude",
		upstreamBase + "/" + Version + "/" + string(linuxARM64) + "/claude",
	}
	sort.Strings(expected)
	if served := filesBelow(t, repo.upstream); strings.Join(served, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("the served tree is %v, not the canonical request set %v", served, expected)
	}
	lookalike := filepath.Join(scratch(t, "writer-lookalike"), "blob")
	harness.WriteFile(t, lookalike,
		[]byte("#!/bin/sh\n# not the pinned release, from a lookalike\n"))
	for _, relative := range expected {
		harness.CopyFile(t, lookalike, filepath.Join(repo.upstream, strings.Replace(relative,
			"downloads.claude.ai", "downloads.claude.ai.evil.example", 1)))
	}
	result := repo.run(t, flags("--version", Version))
	result.RequireStatus(t, 0)
	pin := repo.pin(t)
	for _, name := range platforms {
		pinned := object(t, pin, "platforms", name.key)
		url, ok := pinned["url"].(string)
		if !ok || !strings.HasPrefix(url, "https://"+upstreamBase+"/"+Version+"/") {
			t.Fatalf("%s URL left the canonical host: %v", name.key, pinned["url"])
		}
		if fmt.Sprint(pinned["sha256"]) != platformDigest(t, repo.executables[name.token]) {
			t.Fatalf("%s is not the executable served on the canonical host", name.key)
		}
	}
	// The initial evidence requests come before any payload: a run whose manifest is
	// missing refuses without publishing a byte.
	broken := newWriterRepo(t, scratch(t, "writer-fetch-order"))
	remove(t, broken.published(upstreamBase+"/"+Version+"/"+manifestName))
	refused := broken.run(t, flags("--version", Version))
	refused.RequireStatus(t, 1)
	refused.RequireStderr(t, upstreamBase+"/"+Version+"/"+manifestName)
	if published := publishedFiles(refused.Stdout); len(published) != 0 {
		t.Fatalf("a run with no manifest published %v", published)
	}
}

// TestWriterCheckAcceptsACommittedClosureAndRefusesTampering: `--check` re-hashes what is
// committed, and a mutated evidence byte is a refusal, not a warning.
//
// Both runs go through `script/dev/pin-claude.sh`, because `--check` is the flag the
// wrapper's own forwarding could lose and every other case here drives the binary the
// wrapper builds.
func TestWriterCheckAcceptsACommittedClosureAndRefusesTampering(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-check"))
	result := repo.runWrapper(t, flags("--check"))
	result.RequireStatus(t, 0)
	result.RequireStdout(t, "offline committed-input check")
	result.RequireStdout(t, string(testFixture(t).signer.Fingerprint))

	path := repo.evidencePath(manifestName)
	raw := harness.ReadFile(t, path)
	raw[0] ^= 0xFF
	harness.WriteFile(t, path, raw)

	refused := repo.runWrapper(t, flags("--check"))
	refused.RequireStatus(t, 1)
	refused.RequireStderr(t, "does not match the pin digest")
}

// TestWriterCheckAcceptsTheRealCommittedClosureOffline: `--check` over the real committed
// closure, with the reviewed signer, changes nothing.
func TestWriterCheckAcceptsTheRealCommittedClosureOffline(t *testing.T) {
	base := scratch(t, "writer-committed-check")
	root := filepath.Join(base, "repo")
	committed := committedDirectory(t)
	if _, err := os.Stat(filepath.Join(committed, "pin.json")); err != nil {
		t.Skipf("no committed Claude pin adopted yet: %v", err)
	}
	for _, relative := range []string{"pin.json", "evidence/" + manifestName,
		"evidence/" + signatureName, keyName} {
		harness.CopyFile(t, filepath.Join(committed, filepath.FromSlash(relative)),
			filepath.Join(root, filepath.FromSlash(pinDirectory+"/"+relative)))
	}
	harness.CopyFile(t, filepath.Join(harness.Repository(t), dockerfile),
		filepath.Join(root, dockerfile))
	repo := &writerRepo{base: base, root: root}
	before := repo.snapshot(t)
	result := repo.run(t, flags("--check"))
	result.RequireStatus(t, 0)
	result.RequireStdout(t, "offline committed-input check")
	result.RequireStdout(t, "not a fresh fetch")
	if after := repo.snapshot(t); !sameSnapshot(after, before) {
		t.Fatalf("--check changed a committed byte: %s", after.difference(before))
	}
}

// TestWriterCheckRequiresOneClaudeARGAndItsLabel: the ARG and its LABEL are how a
// released image says which version its binary is.
func TestWriterCheckRequiresOneClaudeARGAndItsLabel(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-slots"))
	path := filepath.Join(repo.root, dockerfile)
	original := harness.ReadFile(t, path)
	rows := []struct {
		name string
		text string
	}{
		{"two ARG lines", string(original) + "\n" + dockerfileArg + Version + "\n"},
		{"label missing", strings.Replace(string(original),
			`LABEL org.agent-vm.version.claude="${AGENT_VERSION_CLAUDE}"`+"\n", "", 1)},
		{"ARG differs", strings.Replace(string(original), dockerfileArg+Version,
			dockerfileArg+"0.0.0", 1)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			harness.WriteFile(t, path, []byte(row.text))
			result := repo.run(t, flags("--check"))
			result.RequireStatus(t, 1)
			result.RequireStderr(t, "Dockerfile must declare")
		})
	}
	harness.WriteFile(t, path, original)
}

// TestWriterMissingCommittedReleaseKeyRefused: the release key is the trust anchor and
// is committed by hand.
func TestWriterMissingCommittedReleaseKeyRefused(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-no-key"), keyless())
	result := repo.run(t, flags("--version", Version))
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "committed by hand")
}

// TestWriterEvidenceWithoutAPinJSONRefused: a closure with evidence and no pin.json is a
// partial state, not a start.
func TestWriterEvidenceWithoutAPinJSONRefused(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-partial"), unadopted())
	harness.WriteFile(t, filepath.Join(repo.root, evidenceDirectory, manifestName), []byte("{}\n"))
	result := repo.run(t, flags("--version", Version))
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "refusing a partial old state")
}

// TestWriterUsageErrorsExitTwo: a malformed invocation is a usage error, not a refusal.
func TestWriterUsageErrorsExitTwo(t *testing.T) {
	repo := newWriterRepo(t, scratch(t, "writer-usage"))
	for _, arguments := range [][]string{{"--version", Version, "--check"}, nil} {
		result := repo.run(t, flags(arguments...))
		result.RequireStatus(t, 2)
	}
}
