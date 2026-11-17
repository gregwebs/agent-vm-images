package agywriter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
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

// TestRefreshRetainsTheOwnerScratchWhenPublicationIsIncomplete: a status-3
// publication is undecided, so the owner's own scratch is kept and printed together
// with the publisher's, and it still holds exactly the five proposed files. The
// repository is untouched: nothing was published.
func TestRefreshRetainsTheOwnerScratchWhenPublicationIsIncomplete(t *testing.T) {
	fx := newFixture(t)
	root := fixtureRoot(t, fixtureVersion)
	client, out, errOut := newTestClient(t, root)
	client.Fetch = fx.fetcher()
	var published publication.Request
	client.Publish = func(request publication.Request) int {
		published = request
		return 3
	}
	if status := client.Refresh(context.Background(), Options{Version: fx.version}); status != 3 {
		t.Fatalf("refresh: status %d, want 3\n%s", status, errOut)
	}
	if out.Len() != 0 {
		t.Errorf("an undecided publication reported success: %s", out)
	}
	transcript := errOut.String()
	announced := pathAfter(t, transcript, "owner scratch: ")
	retained := pathAfter(t, transcript, "owner scratch retained at ")
	if announced != retained {
		t.Errorf("the scratch was announced at %s and retained at %s", announced, retained)
	}
	if !filepath.IsAbs(retained) {
		t.Errorf("the retained scratch is not absolute: %s", retained)
	}
	if _, err := os.Stat(retained); err != nil {
		t.Errorf("the retained scratch is gone: %s", err)
	}
	if !strings.Contains(transcript, "STOP: recover "+retained) {
		t.Errorf("the transcript does not tell a maintainer how to recover:\n%s", transcript)
	}
	if published.Prefix != Prefix || published.Root != root || published.Owner != announced {
		t.Errorf("the publication was asked for %s in %s by %s", published.Prefix,
			published.Root, published.Owner)
	}
	paths := make([]string, 0, len(published.Files))
	for _, entry := range published.Files {
		paths = append(paths, entry.Path)
		if raw, err := os.ReadFile(filepath.Join(retained, "proposal", entry.Path)); err != nil ||
			!bytes.Equal(raw, entry.Raw) {
			t.Errorf("the retained scratch does not hold the staged %s: %v", entry.Path, err)
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
		t.Errorf("an undecided publication changed the repository: %v", files)
	}
}

// TestRefreshReportsAFailedPublication: a status-1 publication is fully restored, so
// the owner scratch is removed again and nothing is retained. The announced
// directory is where a signal handler would have pointed, so it is the one to check.
func TestRefreshReportsAFailedPublication(t *testing.T) {
	fx := newFixture(t)
	root := fixtureRoot(t, fixtureVersion)
	client, out, errOut := newTestClient(t, root)
	client.Fetch = fx.fetcher()
	client.Publish = func(publication.Request) int { return 1 }
	if status := client.Refresh(context.Background(), Options{Version: fx.version}); status != 1 {
		t.Fatalf("refresh: status %d, want 1\n%s", status, errOut)
	}
	if out.Len() != 0 {
		t.Errorf("a failed publication reported success: %s", out)
	}
	transcript := errOut.String()
	if strings.Contains(transcript, "retained at") {
		t.Errorf("a restored publication retained its scratch:\n%s", transcript)
	}
	announced := pathAfter(t, transcript, "owner scratch: ")
	if _, err := os.Stat(announced); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed run left %s behind: %v", announced, err)
	}
	if files := snapshot(t, root); len(files) != 1 {
		t.Errorf("a failed publication changed the repository: %v", files)
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
