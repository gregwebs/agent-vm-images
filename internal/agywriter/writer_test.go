package agywriter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// TestRefreshRefusesGitHubActionsBeforeAnyFetch: the CI refusal is the first thing a
// write run does. The fetcher counts requests, so this is the difference between a
// refusal that happened before the first request and one that merely stayed silent.
func TestRefreshRefusesGitHubActionsBeforeAnyFetch(t *testing.T) {
	fx := newFixture(t)
	root := fixtureRoot(t, fixtureVersion)
	client, out, errOut := newTestClient(t, root)
	client.Env.Variables["GITHUB_ACTIONS"] = "true"
	fetcher := fx.fetcher()
	client.Fetch = fetcher
	if status := client.Refresh(context.Background(), Options{Version: fx.version}); status != 1 {
		t.Fatalf("refresh: status %d", status)
	}
	if len(fetcher.fetched) != 0 {
		t.Errorf("a refused CI run fetched %v", fetcher.fetched)
	}
	if out.Len() != 0 {
		t.Errorf("the refusal wrote to stdout: %s", out)
	}
	if !strings.Contains(errOut.String(), "refusing GitHub Actions") {
		t.Errorf("the refusal does not name GitHub Actions: %s", errOut)
	}
	if files := snapshot(t, root); len(files) != 1 {
		t.Errorf("a refused CI run left %v", files)
	}
	if _, err := os.Stat(filepath.Join(root, PinDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused CI run created %s", PinDirectory)
	}
}

// TestRefreshLeavesTheOwnerScratchToThePublisher: the publisher owns this
// invocation's scratch on every status it can return — it removes it on a decided
// outcome and retains and prints it otherwise — so the writer must not remove it and
// must not report an outcome the publisher has not reported.
//
// Every row reddens if a writer removes its own scratch: the announced directory is
// gone, and an undecided publication has lost the proposal its backups belong to.
func TestRefreshLeavesTheOwnerScratchToThePublisher(t *testing.T) {
	for _, status := range []int{0, 1, 3} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			fx := newFixture(t)
			root := fixtureRoot(t, fixtureVersion)
			client, out, errOut := newTestClient(t, root)
			client.Fetch = fx.fetcher()
			var published publication.Request
			client.Publish = func(request publication.Request) int {
				published = request
				return status
			}
			if got := client.Refresh(context.Background(), Options{Version: fx.version}); got != status {
				t.Fatalf("refresh: status %d, want %d\n%s", got, status, errOut)
			}
			if out.Len() != 0 {
				t.Errorf("the writer reported an outcome the publisher owns: %s", out)
			}
			transcript := errOut.String()
			announced := pathAfter(t, transcript, "owner scratch: ")
			if !filepath.IsAbs(announced) {
				t.Errorf("the announced scratch is not absolute: %s", announced)
			}
			if _, err := os.Stat(announced); err != nil {
				t.Errorf("the writer removed its own scratch: %s", err)
			}
			if strings.Contains(transcript, "retained at") || strings.Contains(transcript, "STOP:") {
				t.Errorf("the writer claims a retention the publisher decides:\n%s", transcript)
			}
			if published.Prefix != Prefix || published.Root != root || published.Owner != announced {
				t.Errorf("the publication was asked for %s in %s by %s", published.Prefix,
					published.Root, published.Owner)
			}
			paths := make([]string, 0, len(published.Files))
			for _, entry := range published.Files {
				paths = append(paths, entry.Path)
				if raw, err := os.ReadFile(filepath.Join(announced, "proposal", entry.Path)); err != nil ||
					!bytes.Equal(raw, entry.Raw) {
					t.Errorf("the handed-over scratch does not hold the staged %s: %v", entry.Path, err)
				}
			}
			sort.Strings(paths)
			wanted := []string{
				PinFile,
				PinDirectory + "/" + artifactpin.AgyReleaseEvidence.String(),
				PinDirectory + "/" + artifactpin.AgyEvidence[artifactpin.LinuxAMD64].String(),
				PinDirectory + "/" + artifactpin.AgyEvidence[artifactpin.LinuxARM64].String(),
				Dockerfile,
			}
			sort.Strings(wanted)
			if strings.Join(paths, ",") != strings.Join(wanted, ",") {
				t.Errorf("the publication carries %v", paths)
			}
			if files := snapshot(t, root); len(files) != 1 {
				t.Errorf("a published request changed the repository: %v", files)
			}
		})
	}
}

// pathAfter is the absolute path one contract line names.
func pathAfter(t *testing.T, transcript, marker string) string {
	t.Helper()
	for _, line := range strings.Split(transcript, "\n") {
		if index := strings.Index(line, marker); index >= 0 {
			return strings.TrimSpace(line[index+len(marker):])
		}
	}
	t.Fatalf("the transcript has no %q line:\n%s", marker, transcript)
	return ""
}
