package agy

import (
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

// zones are the two extreme UTC offsets, in opposite directions, so between them
// every hour of the UTC day has one host whose local date differs from the UTC
// date. Running both is what makes a writer that stamps the host's local date fail
// at every hour instead of at some of them.
var zones = []struct {
	name   string
	offset time.Duration
}{
	{"Pacific/Kiritimati", 14 * time.Hour},
	{"Etc/GMT+12", -12 * time.Hour},
}

func sortedDestinations() []string {
	sorted := append([]string(nil), Destinations...)
	sort.Strings(sorted)
	return sorted
}

// treeNames is a tree's file names, sorted, for a message that has to say what a
// publication wrote.
func treeNames(tree map[string][]byte) []string {
	names := make([]string, 0, len(tree))
	for name := range tree {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func requireTree(t *testing.T, repo *repository) {
	t.Helper()
	tree := repo.tree(t)
	if strings.Join(treeNames(tree), "\n") != strings.Join(sortedDestinations(), "\n") {
		t.Fatalf("the publication wrote %v, want %v", treeNames(tree), sortedDestinations())
	}
}

// TestAFirstAdoptionPublishesTheFiveFiles holds a first adoption to the frozen
// transcript and the five destinations.
//
// Maintainers and the other suites read these lines: the published summary names
// the destinations in sorted order, the second line names the file that must then
// be updated, and the owner scratch is named absolutely and removed by a run that
// published.
func TestAFirstAdoptionPublishesTheFiveFiles(t *testing.T) {
	work := harness.TempDir(t, "agy-transcript.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))
	before := repo.state(t)

	result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
	requireStatus(t, result, 0)

	lines := strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n")
	want := "pin-agy: published agy " + Version + ": " + strings.Join(sortedDestinations(), ", ")
	if lines[0] != want {
		t.Fatalf("the published line is %q, want %q", lines[0], want)
	}
	if len(lines) != 2 {
		t.Fatalf("the published run printed %q", lines)
	}
	if !strings.Contains(lines[1], "pin-agy: now update script/test/fixtures/installer-egress/selections.tsv") {
		t.Fatalf("the transcript never names the selections file: %q", lines)
	}

	var announced []string
	for _, line := range strings.Split(result.Stderr, "\n") {
		if strings.Contains(line, "owner scratch: ") {
			announced = append(announced, line)
		}
	}
	if len(announced) != 1 {
		t.Fatalf("the run announced %d scratch directories\n%s", len(announced), result.Report())
	}
	scratch := strings.TrimSpace(strings.SplitN(announced[0], "owner scratch: ", 2)[1])
	if !filepath.IsAbs(scratch) {
		t.Fatalf("the scratch is not absolute: %s", scratch)
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Fatalf("a published run kept its scratch: %s", scratch)
	}
	if strings.Contains(result.Stderr, "retained") {
		t.Fatalf("a published run retained material\n%s", result.Report())
	}

	requireTree(t, repo)
	if sameState(repo.state(t), before) {
		t.Fatal("the publication wrote nothing")
	}
	pin := document(t, repo.read(t, PinFile))
	if pin["version"] != Version {
		t.Fatalf("the pin names release %v, want %s", pin["version"], Version)
	}
	if !strings.Contains(string(repo.read(t, Dockerfile)), "ARG AGENT_VERSION_AGY="+Version) {
		t.Fatal("the Dockerfile ARG was not written")
	}
}

// TestTheMaintainerWrapperRunsTheWriter drives `script/dev/pin-agy.sh --root DIR`,
// which is the path CONTRIBUTING names. Every other case calls the binary the
// wrapper builds, so this is the one place the wrapper's own three lines (compile,
// `cd` to the checkout, `exec avm-pin agy`) are exercised.
func TestTheMaintainerWrapperRunsTheWriter(t *testing.T) {
	work := harness.TempDir(t, "agy-wrapper.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))

	result := runWrapper(t, repo, arguments("--version", Version), serving(upstream))
	requireStatus(t, result, 0)
	if !strings.Contains(result.Stdout, "pin-agy: published agy "+Version+":") {
		t.Fatalf("the wrapper did not run the writer\n%s", result.Report())
	}
	requireTree(t, repo)
}

// TestTheDecisionTable is every row of the pin tool's decision table, each with its
// own fixture: one channel agreeing with the other is the whole trust decision, so
// every way they can disagree gets a fixture where only it can be the reason.
func TestTheDecisionTable(t *testing.T) {
	rows := []struct {
		name   string
		mutate func(*release)
		needle string
	}{
		{
			name:   "both channels agree",
			mutate: func(*release) {},
		},
		{
			name:   "manifests disagree",
			mutate: func(served *release) { served.manifestVersions[linuxARM64] = Newer },
			needle: "name linux/amd64",
		},
		{
			name: "manifests agree but not with --version",
			mutate: func(served *release) {
				for _, platform := range platforms {
					served.manifestVersions[platform] = Older
				}
			},
			needle: "only the release they",
		},
		{
			name: "GitHub digest disagrees",
			mutate: func(served *release) {
				served.releaseDigests[linuxAMD64] = "sha256:" + strings.Repeat("0", 64)
			},
			needle: "but the downloaded bytes are",
		},
		{
			name: "manifest sha512 disagrees",
			mutate: func(served *release) {
				served.manifestSHA512[linuxARM64] = strings.Repeat("0", 128)
			},
			needle: "manifest records sha512",
		},
		{
			name:   "GitHub omits the asset",
			mutate: func(served *release) { served.dropAssets[linuxARM64] = true },
			needle: "does not list",
		},
		{
			name:   "release names another tag",
			mutate: func(served *release) { served.releaseTag = Older },
			needle: "the pin says",
		},
		{
			name:   "release is a draft",
			mutate: func(served *release) { served.draft = true },
			needle: "not a published release",
		},
	}
	for index, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			work := harness.TempDir(t, fmt.Sprintf("agy-decision-%d.", index))
			repo := newRepository(t, filepath.Join(work, "repo"), Version)
			served := newRelease(t, Version)
			row.mutate(served)
			upstream := served.writeServed(t, filepath.Join(work, "upstream"))
			before := repo.state(t)

			result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
			if row.needle == "" {
				requireStatus(t, result, 0)
				requireTree(t, repo)
				if !strings.Contains(string(repo.read(t, Dockerfile)), "ARG AGENT_VERSION_AGY="+Version) {
					t.Fatal("the Dockerfile ARG was not written")
				}
				return
			}
			requireRefusal(t, result, row.needle, repo, before)
		})
	}
}

// TestAPlatformIsCheckedAgainstItsOwnManifest proves a platform is only ever
// checked against its own manifest: the amd64 sha512 planted in the arm64 record
// must not corroborate the arm64 archive.
func TestAPlatformIsCheckedAgainstItsOwnManifest(t *testing.T) {
	work := harness.TempDir(t, "agy-cross-manifest.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	served := newRelease(t, Version)
	other := newRelease(t, Version)
	served.manifestSHA512[linuxARM64] = other.manifestSHA512[linuxAMD64]
	if served.manifestURLs[linuxAMD64] == served.manifestURLs[linuxARM64] {
		t.Fatal("the fixture urls are equal, so the row proves nothing")
	}
	upstream := served.writeServed(t, filepath.Join(work, "upstream"))
	before := repo.state(t)

	result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
	requireRefusal(t, result, "manifest records sha512", repo, before)
}

// TestAWrongArchitectureBinaryIsRefused is consistent hashes on both channels and
// an archive that holds the other machine: the digest chain is intact and the
// binary would still be the wrong one.
func TestAWrongArchitectureBinaryIsRefused(t *testing.T) {
	work := harness.TempDir(t, "agy-wrong-elf.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	served := newRelease(t, Version)
	// An amd64 row whose archive is the arm64 machine: consistent digests on both
	// channels, and the binary is still the wrong one.
	served.setBody(t, linuxAMD64, binary(t, platformSpellings[linuxARM64].machine, reviewedMode, nil))
	upstream := served.writeServed(t, filepath.Join(work, "upstream"))
	before := repo.state(t)

	result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
	requireRefusal(t, result, "ELF machine 183", repo, before)
}

// TestTheArchiveMemberModes holds the archive's own permission bits to the review.
//
// Both channels can corroborate an archive whose single member is mode 0777, so the
// digest chain says nothing about who may write /usr/local/bin/agy. The pin-time
// preflight shares its checks with the install, so refusing here is the same rule
// the install applies.
func TestTheArchiveMemberModes(t *testing.T) {
	for _, mode := range []int64{0o777, 0o775, 0o700} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			work := harness.TempDir(t, fmt.Sprintf("agy-member-mode-%04o.", mode))
			repo := newRepository(t, filepath.Join(work, "repo"), Version)
			served := newRelease(t, Version)
			for _, platform := range platforms {
				served.setBody(t, platform, binary(t, platform.spell(t).machine, mode, nil))
			}
			upstream := served.writeServed(t, filepath.Join(work, "upstream"))
			before := repo.state(t)

			result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
			requireRefusal(t, result, fmt.Sprintf("is mode %04o", mode), repo, before)
		})
	}
}

// TestTheCommittedSecondPlatformIsCorroborated holds the committed corroboration to
// the platform that is not being built.
//
// A loop narrowed to the first platform would let half a release drift, so each row
// tampers the arm64 record — and re-hashes it into the pin, or the row would only
// prove the evidence hash — and then asks for a bump. The served tree has no
// archive at all, so the expected refusal can only be the corroboration: a payload
// fetch would report its own, different failure.
func TestTheCommittedSecondPlatformIsCorroborated(t *testing.T) {
	golden := harness.TempDir(t, "agy-second-platform.")
	committed := published(t, newRepository(t, filepath.Join(golden, "repo"), Version), newRelease(t, Version), golden, Version)

	// retarget re-encodes the pin after mutating it, so a tampered evidence byte is
	// also the byte the pin names.
	retarget := func(t *testing.T, repo *repository, mutate func(map[string]any)) {
		t.Helper()
		pin := document(t, repo.read(t, PinFile))
		mutate(pin)
		repo.write(t, PinFile, encodeDocument(t, pin))
	}
	rewrite := func(t *testing.T, repo *repository, relative string, mutate func(map[string]any)) {
		t.Helper()
		doc := document(t, repo.read(t, relative))
		mutate(doc)
		repo.write(t, relative, encodeDocument(t, doc))
	}
	rehashManifest := func(t *testing.T, repo *repository, platform platform) {
		t.Helper()
		evidence := manifestEvidence(t, platform)
		digest := harness.SHA256Hex(repo.read(t, evidence))
		retarget(t, repo, func(pin map[string]any) {
			authentication := object(t, pin, "authentication")
			manifests := object(t, authentication, "manifests")
			object(t, manifests, string(platform))["sha256"] = digest
		})
	}
	rehashRelease := func(t *testing.T, repo *repository) {
		t.Helper()
		digest := harness.SHA256Hex(repo.read(t, PinDirectory+"/evidence/release.json"))
		retarget(t, repo, func(pin map[string]any) {
			object(t, object(t, pin, "authentication"), "release")["sha256"] = digest
		})
	}
	stripArchives := func(t *testing.T, upstream string) {
		t.Helper()
		for _, platform := range platforms {
			if err := os.Remove(servedPath(t, upstream, assetURL(Version, assetName(t, platform)))); err != nil {
				t.Fatalf("cannot strip the %s archive: %v", platform, err)
			}
		}
	}
	refused := func(t *testing.T, repo *repository, needle string, before state, work string) {
		t.Helper()
		upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))
		stripArchives(t, upstream)
		result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
		requireRefusal(t, result, needle, repo, before)
		if strings.Contains(result.Stdout+result.Stderr, "cannot fetch") {
			t.Fatalf("a payload was fetched before the corroboration\n%s", result.Report())
		}
	}

	t.Run("the arm64 manifest", func(t *testing.T) {
		work := harness.TempDir(t, "agy-second-platform-manifest.")
		repo := newRepository(t, filepath.Join(work, "repo"), Version).install(t, committed)
		rewrite(t, repo, PinDirectory+"/evidence/manifest-linux-arm64.json", func(doc map[string]any) {
			doc["sha512"] = strings.Repeat("0", 128)
		})
		rehashManifest(t, repo, linuxARM64)
		refused(t, repo, "the pin says", repo.state(t), work)
	})

	t.Run("the arm64 release digest", func(t *testing.T) {
		work := harness.TempDir(t, "agy-second-platform-release.")
		repo := newRepository(t, filepath.Join(work, "repo"), Version).install(t, committed)
		target := linuxARM64
		rewrite(t, repo, PinDirectory+"/evidence/release.json", func(doc map[string]any) {
			for _, entry := range list(t, doc, "assets") {
				asset, ok := entry.(map[string]any)
				if !ok {
					t.Fatalf("a release asset is not an object: %T", entry)
				}
				if asset["name"] == assetName(t, target) {
					asset["digest"] = "sha256:" + strings.Repeat("0", 64)
				}
			}
		})
		rehashRelease(t, repo)
		refused(t, repo, "the pin says", repo.state(t), work)
	})
}

// TestLatestOnlyAndTheSameReleaseRewrite holds the two rules that keep a daily run
// quiet: a release older than the pin is not published, and a release whose served
// bytes changed under the same version is not the release the pin records.
func TestLatestOnlyAndTheSameReleaseRewrite(t *testing.T) {
	golden := harness.TempDir(t, "agy-golden.")
	committed := published(t, newRepository(t, filepath.Join(golden, "repo"), Version), newRelease(t, Version), golden, Version)

	t.Run("older than the pinned release", func(t *testing.T) {
		work := harness.TempDir(t, "agy-older-manifest.")
		repo := newRepository(t, filepath.Join(work, "repo"), Version).install(t, committed)
		upstream := newRelease(t, Older).writeServed(t, filepath.Join(work, "upstream"))
		// Before the run, never after it: a tree read after the writer returned
		// already contains whatever the writer wrote, so comparing it with itself
		// would pass even when the refusal damaged the sources.
		before := repo.state(t)
		result := runWriter(t, repo, arguments("--version", Older), serving(upstream))
		requireRefusal(t, result, "older than the pinned "+Version, repo, before)
	})

	t.Run("a same-release upstream rewrite", func(t *testing.T) {
		work := harness.TempDir(t, "agy-same-release-rewrite.")
		repo := newRepository(t, filepath.Join(work, "repo"), Version).install(t, committed)
		rewritten := newRelease(t, Version)
		rewritten.setBody(t, linuxAMD64, binary(t, platformSpellings[linuxAMD64].machine, reviewedMode, append(elfHeader(62), "# rewritten\n"...)))
		upstream := rewritten.writeServed(t, filepath.Join(work, "upstream"))
		before := repo.state(t)
		result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
		requireRefusal(t, result, "same-release upstream rewrite", repo, before)
	})
}

// TestARepeatRunIsByteIdentical: a second run of the same release keeps the
// committed evidence bytes, so a daily download-count change is not a diff.
func TestARepeatRunIsByteIdentical(t *testing.T) {
	work := harness.TempDir(t, "agy-repeat.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	upstream := filepath.Join(work, "upstream")

	served := newRelease(t, Version)
	served.downloadCount = 4111
	result := runWriter(t, repo, arguments("--version", Version), serving(served.writeServed(t, upstream)))
	requireStatus(t, result, 0)
	before := repo.state(t)

	// Same release, new download count on every asset: the security projection is
	// unchanged, so every committed byte must survive.
	bumped := newRelease(t, Version)
	bumped.downloadCount = 9999
	result = runWriter(t, repo, arguments("--version", Version), serving(bumped.writeServed(t, upstream)))
	requireStatus(t, result, 0)
	if !strings.Contains(result.Stdout, "unchanged") {
		t.Fatalf("a repeat run did not report unchanged\n%s", result.Report())
	}
	if !sameState(repo.state(t), before) {
		t.Fatal("a repeat run changed the tree")
	}

	// One platform's manifest is served in another encoding but parses to the same
	// document: every committed evidence byte must still be kept verbatim.
	if err := os.RemoveAll(upstream); err != nil {
		t.Fatalf("cannot clear the upstream: %v", err)
	}
	reformatted := newRelease(t, Version)
	reformatted.downloadCount = 9999
	servedDir := reformatted.writeServed(t, upstream)
	path := servedPath(t, servedDir, manifestURL(t, linuxAMD64))
	harness.WriteFile(t, path, encodeIndented(t, document(t, harness.ReadFile(t, path)), "    "))
	result = runWriter(t, repo, arguments("--version", Version), serving(servedDir))
	requireStatus(t, result, 0)
	if !strings.Contains(result.Stdout, "unchanged") {
		t.Fatalf("a reformatted manifest is not a change\n%s", result.Report())
	}
	if !sameState(repo.state(t), before) {
		t.Fatal("a reformatted manifest changed the committed evidence")
	}
}

// TestThePinDateIsTheUTCDate: `verified_on` is the UTC date, and a repeat keeps the
// committed date instead of moving it to today's.
func TestThePinDateIsTheUTCDate(t *testing.T) {
	now := time.Now().UTC()
	var differing string
	utc := now.Format(time.DateOnly)
	for _, zone := range zones {
		// A host at this offset reads a local date; only one whose date differs from
		// the UTC date can catch a writer that stamps the local one.
		if now.Add(zone.offset).Format(time.DateOnly) != utc {
			differing = zone.name
			break
		}
	}
	if differing == "" {
		t.Fatalf("neither zone has a local date other than the UTC date: %v", zones)
	}

	for _, zone := range zones {
		t.Run(zone.name, func(t *testing.T) {
			work := harness.TempDir(t, "agy-clock.")
			repo := newRepository(t, filepath.Join(work, "repo"), Version)
			upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))
			before := time.Now().UTC().Format(time.DateOnly)
			result := runWriter(t, repo, arguments("--version", Version), serving(upstream), withEnv("TZ", zone.name))
			after := time.Now().UTC().Format(time.DateOnly)
			requireStatus(t, result, 0)

			pin := document(t, repo.read(t, PinFile))
			if _, ok := object(t, pin, "authentication")["verified_on"].(string); !ok {
				t.Fatal("the pin carries no verified_on date")
			}
			stamped := object(t, pin, "authentication")["verified_on"]
			// A run that straddles UTC midnight legitimately stamps either date.
			if stamped != before && stamped != after {
				t.Fatalf("a writer under TZ=%s stamped %v, not the UTC date %s", zone.name, stamped, after)
			}
		})
	}

	// A same-release repeat keeps the committed `verified_on` instead of moving it to
	// today's UTC date, so a UTC stamp cannot perturb a byte-identical re-pin.
	t.Run("a repeat keeps the committed date", func(t *testing.T) {
		work := harness.TempDir(t, "agy-clock-repeat.")
		repo := newRepository(t, filepath.Join(work, "repo"), Version)
		upstream := filepath.Join(work, "upstream")
		served := newRelease(t, Version)
		served.downloadCount = 4111
		result := runWriter(t, repo, arguments("--version", Version),
			serving(served.writeServed(t, upstream)), withEnv("TZ", differing))
		requireStatus(t, result, 0)

		raw := repo.read(t, PinFile)
		published := document(t, raw)
		on, ok := object(t, published, "authentication")["verified_on"].(string)
		if !ok {
			t.Fatal("the committed pin carries no verified_on date")
		}
		field := []byte(fmt.Sprintf("%q: %q", "verified_on", on))
		if count := bytes.Count(raw, field); count != 1 {
			t.Fatalf("the committed pin carries %d verified_on fields", count)
		}
		pinned := "2026-01-02"
		repo.write(t, PinFile, bytes.Replace(raw, field, []byte(fmt.Sprintf("%q: %q", "verified_on", pinned)), 1))

		before := repo.state(t)
		bumped := newRelease(t, Version)
		bumped.downloadCount = 9999
		result = runWriter(t, repo, arguments("--version", Version),
			serving(bumped.writeServed(t, upstream)), withEnv("TZ", differing))
		requireStatus(t, result, 0)
		if !strings.Contains(result.Stdout, "unchanged") {
			t.Fatalf("a dated same-release repeat is not a change\n%s", result.Report())
		}
		if !sameState(repo.state(t), before) {
			t.Fatal("a dated same-release repeat changed the tree")
		}
	})
}

// TestAdoptionAndTheDockerfileShapes: a partial committed directory, and the three
// Dockerfile shapes the writer refuses. Every row snapshots the sources before the
// writer runs, so a write made just before the refusal is a failure rather than the
// comparison's own baseline.
func TestAdoptionAndTheDockerfileShapes(t *testing.T) {
	t.Run("a partial committed directory", func(t *testing.T) {
		work := harness.TempDir(t, "agy-partial.")
		repo := newRepository(t, filepath.Join(work, "repo"), Version)
		repo.write(t, PinDirectory+"/evidence/release.json", []byte("{}\n"))
		upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))
		before := repo.state(t)
		result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
		requireRefusal(t, result, "with no pin.json", repo, before)
	})

	rows := []struct {
		name   string
		text   string
		needle string
	}{
		{
			name:   "no ARG",
			text:   strings.Replace(dockerfileText(Version), "ARG AGENT_VERSION_AGY=", "ARG OTHER=", 1),
			needle: "exactly one `ARG AGENT_VERSION_AGY=",
		},
		{
			name:   "two ARGs",
			text:   dockerfileText(Version) + "\nARG AGENT_VERSION_AGY=0.0.1\n",
			needle: "exactly one `ARG AGENT_VERSION_AGY=",
		},
		{
			name: "no label",
			text: strings.Replace(dockerfileText(Version),
				"LABEL org.agent-vm.version.agy=\"${AGENT_VERSION_AGY}\"",
				"LABEL org.agent-vm.version.other=\"x\"", 1),
			needle: "LABEL org.agent-vm.version.agy",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			work := harness.TempDir(t, "agy-dockerfile.")
			repo := newRepository(t, filepath.Join(work, "repo"), Version)
			repo.write(t, Dockerfile, []byte(row.text))
			upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))
			before := repo.state(t)
			result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
			requireRefusal(t, result, row.needle, repo, before)
		})
	}
}

// TestTheCIRefusalPrecedesTheFetch: a write run refuses CI before its first fetch,
// and `--check` still reads.
//
// The write row is served an empty tree, so a fetch before the refusal would fail
// with its own, different message; the served directory is compared too, because a
// refusal that wrote into it did not happen before the request.
func TestTheCIRefusalPrecedesTheFetch(t *testing.T) {
	work := harness.TempDir(t, "agy-ci.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	published(t, repo, newRelease(t, Version), work, Version)
	before := repo.state(t)

	empty := harness.Scratch(t, work, "empty-upstream")
	result := runWriter(t, repo, arguments("--version", Newer), serving(empty), withEnv("GITHUB_ACTIONS", "true"))
	requireRefusal(t, result, "refusing GitHub Actions", repo, before)
	if strings.Contains(result.Stdout+result.Stderr, "cannot fetch") {
		t.Fatalf("a fetch was attempted before the CI refusal\n%s", result.Report())
	}
	if census := harness.Census(t, empty); len(census) != 0 {
		t.Fatalf("a refused CI run wrote into the served tree: %v", census)
	}

	// --check is the offline reader: CI may run it, and the marker changes nothing.
	checked := runWriter(t, repo, arguments("--check"), withEnv("GITHUB_ACTIONS", "true"))
	requireStatus(t, checked, 0)
	checked.RequireStdout(t, "offline committed-input check")
	if strings.Contains(checked.Stderr, "refusing GitHub Actions") {
		t.Fatalf("the CI refusal blocked the read-only check\n%s", checked.Report())
	}
}

// TestCheckReadsOnlyTheCommittedBytes: `--check` re-hashes what is committed, and a
// mutated evidence byte is a refusal, not a warning.
func TestCheckReadsOnlyTheCommittedBytes(t *testing.T) {
	work := harness.TempDir(t, "agy-clean.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	published(t, repo, newRelease(t, Version), work, Version)

	result := runWriter(t, repo, arguments("--check"))
	requireStatus(t, result, 0)
	result.RequireStdout(t, "offline committed-input check")
	result.RequireStdout(t, "not a fresh corroboration")

	evidence := PinDirectory + "/evidence/manifest-linux-amd64.json"
	original := repo.read(t, evidence)
	repo.write(t, evidence, bytes.Replace(original, []byte(`"version": "1.4.11"`), []byte(`"version": "1.4.10"`), 1))
	result = runWriter(t, repo, arguments("--check"))
	if result.Status == 0 {
		t.Fatal("--check passed on a mutated evidence byte")
	}
	if !strings.Contains(result.Stdout+result.Stderr, "the pin says") {
		t.Fatalf("--check did not name the disagreement\n%s", result.Report())
	}
}

// TestCheckOnTheCommittedTree: `--check` passes on the committed tree, names its own
// limits, and catches the ARG the Dockerfile has to keep in step.
func TestCheckOnTheCommittedTree(t *testing.T) {
	work := harness.TempDir(t, "agy-check.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	published(t, repo, newRelease(t, Version), work, Version)

	result := runWriter(t, repo, arguments("--check"))
	requireStatus(t, result, 0)
	result.RequireStdout(t, "offline committed-input check")
	result.RequireStdout(t, "not a fresh corroboration")

	dockerfile := string(repo.read(t, Dockerfile))
	repo.write(t, Dockerfile, []byte(strings.Replace(dockerfile,
		"ARG AGENT_VERSION_AGY="+Version, "ARG AGENT_VERSION_AGY="+Older, 1)))
	result = runWriter(t, repo, arguments("--check"))
	if result.Status == 0 {
		t.Fatal("--check passed on an ARG that disagrees with the pin")
	}
	if strings.TrimSpace(result.Stdout) != "" {
		t.Fatalf("a refused check reported the closure\n%s", result.Report())
	}
	result.RequireStderr(t, "ARG AGENT_VERSION_AGY="+Version)
}

// TestEachBadInputGetsItsOwnStatus holds the status a maintainer's script branches
// on, for each kind of bad input.
//
// A version that cannot name a release is a *refusal* (1), not a malformed
// invocation (2): the flag was given and well formed as an argument, and the tool's
// answer is "this cannot be pinned". A missing or contradictory argument is the
// usage case.
func TestEachBadInputGetsItsOwnStatus(t *testing.T) {
	work := harness.TempDir(t, "agy-statuses.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))

	for _, version := range []string{"v1.4.11", "1.4", "latest", "01.4.11", "1.4.11 "} {
		t.Run(version, func(t *testing.T) {
			result := runWriter(t, repo, arguments("--version", version), serving(upstream))
			requireStatus(t, result, 1)
			result.RequireStderr(t, "exact")
			if _, err := os.Stat(filepath.Join(repo.path, PinDirectory)); err == nil {
				t.Fatal("a malformed version wrote the closure")
			}
		})
	}

	t.Run("a contradictory argument", func(t *testing.T) {
		result := runWriter(t, repo, arguments("--check", "--version", Version))
		requireStatus(t, result, 2)
		result.RequireStderr(t, "--check takes only --root")
	})

	t.Run("a missing argument", func(t *testing.T) {
		result := runWriter(t, repo)
		requireStatus(t, result, 2)
		result.RequireStderr(t, "--version is required")
	})
}

// TestAFailedFetchPublishesNothing: a failed manifest fetch publishes nothing and
// names the URL it asked for.
func TestAFailedFetchPublishesNothing(t *testing.T) {
	work := harness.TempDir(t, "agy-transport.")
	repo := newRepository(t, filepath.Join(work, "repo"), Version)
	upstream := newRelease(t, Version).writeServed(t, filepath.Join(work, "upstream"))
	missing := manifestURL(t, linuxARM64)
	if err := os.Remove(servedPath(t, upstream, missing)); err != nil {
		t.Fatalf("cannot remove the %s manifest: %v", missing, err)
	}
	before := repo.state(t)

	result := runWriter(t, repo, arguments("--version", Version), serving(upstream))
	requireRefusal(t, result, "cannot fetch "+missing, repo, before)
}

// TestWriteFixturePublishesTheClosureTheBumperConsumes holds the fixture seam to its
// contract, because `script/test/agent-versions.sh` reads what it wrote and that
// script is not part of the gate.
//
// The layout is the contract: `source/images/tools/agy/` is a real publication (the
// pin and its three evidence files), and `assets/` holds the served documents by the
// names the bumper serves them under. It runs through `harness.Command`, which is
// exactly what `avm-test write-fixture` calls, so the adapter is exercised too.
func TestWriteFixturePublishesTheClosureTheBumperConsumes(t *testing.T) {
	dir := filepath.Join(harness.TempDir(t, "agy-fixture."), "out")
	if err := harness.Command(func(r harness.Reporter) {
		WriteFixture(r, Fixture{Dir: dir, Version: Version})
	}); err != nil {
		t.Fatalf("the fixture builder refused: %v", err)
	}

	for _, name := range append([]string{"pin.json", "evidence/release.json"}, []string{
		"evidence/" + "manifest-linux-amd64.json", "evidence/manifest-linux-arm64.json",
	}...) {
		path := filepath.Join(dir, "source", PinDirectory, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the source closure is missing %s: %v", name, err)
		}
	}
	pin := document(t, harness.ReadFile(t, filepath.Join(dir, "source", PinFile)))
	if pin["version"] != Version {
		t.Fatalf("the fixture pin names %v, want %s", pin["version"], Version)
	}

	want := []string{
		"agy_cli_linux_arm64.tar.gz",
		"agy_cli_linux_x64.tar.gz",
		"linux_amd64.json",
		"linux_arm64.json",
		"release.json",
	}
	got := make([]string, 0, len(want))
	entries, err := os.ReadDir(filepath.Join(dir, "assets"))
	if err != nil {
		t.Fatalf("the fixture wrote no assets directory: %v", err)
	}
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the fixture assets are %v, want %v", got, want)
	}

	// The bumper reuses a fixture directory, so a second write into one is a refusal
	// rather than a merge.
	if err := harness.Command(func(r harness.Reporter) {
		WriteFixture(r, Fixture{Dir: dir, Version: Version})
	}); err == nil {
		t.Fatal("a second fixture write into the same directory was accepted")
	} else if !strings.Contains(err.Error(), "already holds a fixture") {
		t.Fatalf("the refusal does not name the existing fixture: %v", err)
	}
}
