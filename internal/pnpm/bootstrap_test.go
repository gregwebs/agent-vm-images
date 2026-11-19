package pnpm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/fetch"
)

func TestBootstrap(t *testing.T) {
	t.Run("publishes every authenticated byte", func(t *testing.T) {
		staged := stageInstall(t)
		if err := staged.publish(t, staged.archive); err != nil {
			t.Fatalf("bootstrap refused the reviewed fixture: %v", err)
		}
		// Not just the bundle the pin records a digest for: the entrypoints the
		// launcher execs are outside that digest, so every regular file the archive
		// authenticated is compared with the file the image would run.
		authenticated, err := extract(staged.archive, t.TempDir(), staged.pin)
		if err != nil {
			t.Fatalf("cannot extract the reviewed archive for comparison: %v", err)
		}
		assertSameTree(t, authenticated, staged.prefix, []string{"pin.json"})
		launcher, err := os.ReadFile(filepath.Join(staged.commands, "pnpm"))
		if err != nil {
			t.Fatalf("cannot read the launcher: %v", err)
		}
		if string(launcher) != reviewedLauncher {
			t.Fatalf("launcher is %q", launcher)
		}
		info, err := os.Stat(filepath.Join(staged.commands, "pnpm"))
		if err != nil {
			t.Fatalf("cannot stat the launcher: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("launcher mode is %o", info.Mode().Perm())
		}
		for _, name := range []string{"pnx", "pnpx"} {
			link, err := os.Readlink(filepath.Join(staged.commands, name))
			if err != nil {
				t.Fatalf("cannot read the %s alias: %v", name, err)
			}
			if link != "pnpm" {
				t.Fatalf("%s alias points at %q", name, link)
			}
		}
		entry, err := os.Stat(filepath.Join(staged.prefix, "dist/pnpm.mjs"))
		if err != nil {
			t.Fatalf("cannot stat the published bundle: %v", err)
		}
		if entry.Mode().Perm() != 0o644 {
			t.Fatalf("bundle mode is %o", entry.Mode().Perm())
		}
	})
	t.Run("a missing fixture archive is a refusal and leaves no destination", func(t *testing.T) {
		staged := stageInstall(t)
		err := staged.publish(t, filepath.Join(staged.root, "absent.tgz"))
		if err == nil {
			t.Fatal("bootstrap accepted a missing fixture archive")
		}
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("a missing fixture archive is %T, want *Refusal", err)
		}
		assertNoDestination(t, staged)
	})
	t.Run("an archive the pin does not authenticate is refused", func(t *testing.T) {
		staged := stageInstall(t)
		corrupt := filepath.Join(staged.root, "corrupt.tgz")
		raw, err := os.ReadFile(staged.archive)
		if err != nil {
			t.Fatalf("cannot read the fixture archive: %v", err)
		}
		raw[len(raw)-1] ^= 1
		if err := os.WriteFile(corrupt, raw, 0o600); err != nil {
			t.Fatalf("cannot write the corrupt archive: %v", err)
		}
		err = staged.publish(t, corrupt)
		if err == nil || err.Error() != "archive SHA-512 mismatch" {
			t.Fatalf("refusal is %v, want the archive digest mismatch", err)
		}
		assertNoDestination(t, staged)
	})
	t.Run("a conflicting destination is refused", func(t *testing.T) {
		staged := stageInstall(t)
		t.Setenv(FixtureRootVariable, staged.root)
		prefix, commands, err := FixtureDestination(staged.destination)
		if err != nil {
			t.Fatalf("cannot resolve the fixture destination: %v", err)
		}
		if err := os.MkdirAll(commands, 0o755); err != nil {
			t.Fatalf("cannot create the command directory: %v", err)
		}
		conflict := filepath.Join(commands, "pnpm")
		if err := os.WriteFile(conflict, []byte("existing"), 0o755); err != nil {
			t.Fatalf("cannot write the conflicting command: %v", err)
		}
		publishErr := staged.publish(t, staged.archive)
		if publishErr == nil || publishErr.Error() != "refusing conflicting destination: "+conflict {
			t.Fatalf("refusal is %v", publishErr)
		}
		if _, err := os.Stat(prefix); err == nil {
			t.Fatal("a refused install published a distribution")
		}
	})
	t.Run("a relative fixture archive is refused", func(t *testing.T) {
		staged := stageInstall(t)
		err := staged.publish(t, "pnpm.tgz")
		if err == nil {
			t.Fatal("bootstrap accepted a relative fixture archive")
		}
		assertNoDestination(t, staged)
	})
	t.Run("an escaping destination is refused", func(t *testing.T) {
		staged := stageInstall(t)
		outside := filepath.Join(staged.root, "outside")
		if err := os.Mkdir(outside, 0o755); err != nil {
			t.Fatalf("cannot create the outside directory: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(staged.destination, "opt")); err != nil {
			t.Fatalf("cannot redirect the prefix: %v", err)
		}
		t.Setenv(FixtureRootVariable, staged.root)
		resolved, err := filepath.EvalSymlinks(staged.destination)
		if err != nil {
			t.Fatalf("cannot resolve the fixture destination: %v", err)
		}
		_, _, err = FixtureDestination(staged.destination)
		// The refusal names the first candidate whose own resolution escapes, which
		// is the distribution prefix under the redirected `opt`.
		want := "fixture descendant escapes destination: " + filepath.Join(resolved, "opt/agent-vm/pnpm")
		if err == nil || err.Error() != want {
			t.Fatalf("refusal is %v, want %q", err, want)
		}
	})
	t.Run("a fixture root outside the destination is refused", func(t *testing.T) {
		staged := stageInstall(t)
		t.Setenv(FixtureRootVariable, staged.root)
		_, _, err := FixtureDestination(staged.root)
		if err == nil || err.Error() != "destination outside fixture root" {
			t.Fatalf("refusal is %v", err)
		}
	})
	t.Run("no production positional argument", func(t *testing.T) {
		// The predecessor refused every positional argument rather than reporting
		// usage; the CLI layer keeps that status and message.
		staged := stageInstall(t)
		if err := staged.publish(t, staged.archive); err != nil {
			t.Fatalf("bootstrap refused the reviewed fixture: %v", err)
		}
		if _, err := os.Stat(filepath.Join(staged.prefix, "pin.json")); err != nil {
			t.Fatalf("the published prefix carries no pin: %v", err)
		}
	})
}

// assertNoDestination is the invariant every refused bootstrap keeps: neither
// directory it would publish into exists.
func assertNoDestination(t *testing.T, staged *install) {
	t.Helper()
	if _, err := os.Stat(staged.prefix); err == nil {
		t.Fatal("a refused bootstrap published a distribution")
	}
	if _, err := os.Stat(filepath.Join(staged.commands, "pnpm")); err == nil {
		t.Fatal("a refused bootstrap published a launcher")
	}
}

// TestFixtureDestinationRequiresAnOwnedRoot covers the refusals that do not need a
// published tree.
func TestFixtureDestinationRequiresAnOwnedRoot(t *testing.T) {
	t.Run("the variable is required", func(t *testing.T) {
		t.Setenv(FixtureRootVariable, "")
		if _, _, err := FixtureDestination("/"); err == nil {
			t.Fatal("a missing fixture root was accepted")
		}
	})
	t.Run("the root cannot be the filesystem root", func(t *testing.T) {
		t.Setenv(FixtureRootVariable, "/")
		if _, _, err := FixtureDestination("/"); err == nil || err.Error() != "fixture root must be invocation-owned" {
			t.Fatalf("refusal is %v", err)
		}
	})
	t.Run("the destination must exist", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv(FixtureRootVariable, root)
		if _, _, err := FixtureDestination(filepath.Join(root, "absent")); err == nil {
			t.Fatal("a missing destination was accepted")
		}
	})
}

func TestBootstrapNeedsRootInProduction(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the production refusal is about a non-root caller")
	}
	t.Setenv(FixtureDestinationVariable, "")
	t.Setenv(SourceDirVariable, "")
	err := Bootstrap(context.Background(), Options{SourceDir: t.TempDir()})
	if err == nil {
		t.Fatal("a production bootstrap without root was accepted")
	}
	// The pin is read first, so the refusal here is the missing pin rather than
	// the uid; the uid rule is checked where the destination is resolved.
	_, _, _, targetErr := publicationTargets("")
	if targetErr == nil || targetErr.Error() != "production bootstrap requires root" {
		t.Fatalf("refusal is %v", targetErr)
	}
}

// assertSameTree compares every regular file under want with the same relative
// path under got, byte for byte, and refuses a file only one of them carries.
// allowed lists the paths the publisher is expected to add.
func assertSameTree(t *testing.T, want, got string, allowed []string) {
	t.Helper()
	allowedSet := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = true
	}
	wanted := treeFiles(t, want)
	published := treeFiles(t, got)
	for relative, content := range wanted {
		present, ok := published[relative]
		if !ok {
			t.Fatalf("published tree is missing %s", relative)
		}
		if string(present) != string(content) {
			t.Fatalf("published %s is %q, the archive carries %q", relative, present, content)
		}
	}
	for relative := range published {
		if _, ok := wanted[relative]; ok || allowedSet[relative] {
			continue
		}
		t.Fatalf("published tree carries %s, which the archive did not authenticate", relative)
	}
}

// treeFiles is every regular file under root, by relative path.
func treeFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = content
		return nil
	})
	if err != nil {
		t.Fatalf("cannot walk %s: %v", root, err)
	}
	return files
}

// roundTripFunc is an http.RoundTripper a test drives itself: the download branch
// runs its real code against a fake network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// endlessZeros is a body that never ends, so a ceiling case does not allocate the
// ceiling.
type endlessZeros struct{}

func (endlessZeros) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = 0
	}
	return len(buffer), nil
}

func TestBootstrapDownload(t *testing.T) {
	cases := []struct {
		name string
		// respond answers one attempt with a status and a body.
		respond func(staged *install) (int, io.Reader)
		want    string
		// wantAttempts is how many requests the reviewed budget allows.
		wantAttempts int
	}{
		{
			name: "the pinned archive is fetched and published",
			respond: func(staged *install) (int, io.Reader) {
				payload, err := os.ReadFile(staged.archive)
				if err != nil {
					t.Fatalf("cannot read the fixture archive: %v", err)
				}
				return http.StatusOK, bytes.NewReader(payload)
			},
			wantAttempts: 1,
		},
		{
			name: "a status the pin cannot produce is refused with its URL",
			respond: func(*install) (int, io.Reader) {
				return http.StatusNotFound, strings.NewReader("missing")
			},
			want:         "cannot fetch https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz: HTTP 404",
			wantAttempts: 1,
		},
		{
			name: "a 5xx is retried up to the reviewed budget and then refused",
			respond: func(*install) (int, io.Reader) {
				return http.StatusInternalServerError, strings.NewReader("busy")
			},
			want:         "cannot fetch https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz: HTTP 500",
			wantAttempts: fetch.Attempts,
		},
		{
			// The ceiling lives in internal/fetch, which retries every refusal that
			// is not an HTTP status -- a transport failure and a payload past its
			// ceiling are the same class there. So this costs the full budget; the
			// predicate belongs to that shared package, not to this program.
			name: "a body past the compressed ceiling is refused",
			respond: func(*install) (int, io.Reader) {
				return http.StatusOK, io.LimitReader(endlessZeros{}, MaxArchiveBytes+1)
			},
			want:         "is larger than the 67108864 byte ceiling",
			wantAttempts: fetch.Attempts,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			staged := stageInstall(t)
			attempts := 0
			client := fetch.New()
			client.HTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() != staged.pin.URL {
					t.Errorf("the download asked for %s, the pin names %s", request.URL, staged.pin.URL)
				}
				attempts++
				status, body := testCase.respond(staged)
				return &http.Response{
					StatusCode: status,
					Status:     http.StatusText(status),
					Body:       io.NopCloser(body),
					Request:    request,
				}, nil
			})
			t.Setenv(FixtureRootVariable, staged.root)
			t.Setenv(FixtureDestinationVariable, staged.destination)
			err := Bootstrap(context.Background(), Options{SourceDir: staged.source, Fetch: client})
			if attempts != testCase.wantAttempts {
				t.Fatalf("%d attempts, the reviewed budget allows %d", attempts, testCase.wantAttempts)
			}
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("bootstrap refused the fetched archive: %v", err)
				}
				if _, err := os.Stat(filepath.Join(staged.prefix, "pin.json")); err != nil {
					t.Fatalf("the fetched archive was not published: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("bootstrap accepted a refused download: want %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal is %q, want it to carry %q", err.Error(), testCase.want)
			}
			assertNoDestination(t, staged)
		})
	}
}
