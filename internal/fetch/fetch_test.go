package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two HTTP failure modes, through the primitive every download in the
// installers uses. A committed pin may only name an https origin, so a local
// origin cannot appear in one; the transport is what these controls pin down.
func TestMissingAssetIsRefused(t *testing.T) {
	server := httptest.NewServer(http.FileServer(http.Dir(t.TempDir())))
	defer server.Close()
	_, err := New().Get(context.Background(), server.URL+"/absent.tar.gz", 1024)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("a 404 reported %v", err)
	}
}

func TestRefusedConnectionIsRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = New().Get(context.Background(), "http://"+address+"/node.tar.xz", 1024)
	if err == nil || !strings.Contains(err.Error(), "cannot fetch") {
		t.Fatalf("a refused connection reported %v", err)
	}
}

// The ceiling is enforced one byte over, not at the next power of two.
func TestCeilingIsExact(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "archive.tar.xz")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := New()
	if _, err := client.Get(context.Background(), "file://"+path, 10); err != nil {
		t.Fatalf("ten bytes against a ten byte ceiling: %v", err)
	}
	if _, err := client.Get(context.Background(), "file://"+path, 9); err == nil ||
		!strings.Contains(err.Error(), "byte ceiling") {
		t.Fatalf("eleven bytes against a nine byte ceiling reported %v", err)
	}
}

// A retry is for a transport failure, not for a status the pin itself cannot
// produce: a 404 must fail on the first attempt so a missing asset is one fetch.
func TestStatusIsNotRetried(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	if _, err := New().Get(context.Background(), server.URL+"/absent", 1024); err == nil {
		t.Fatal("a 404 was accepted")
	}
	if requests != 1 {
		t.Fatalf("a 404 was requested %d times", requests)
	}
}

// A redirect is the one place a successful status can send the download somewhere
// the pin never named, so the policy is decided before the target is opened.
func TestRedirectIsRefusedOutsideTheAssetHosts(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("elsewhere"))
		}))
	defer elsewhere.Close()
	origin := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/payload", http.StatusFound)
		}))
	defer origin.Close()
	_, err := New().Get(context.Background(), origin.URL+"/node.tar.xz", 1024)
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Fatalf("a redirect to a host the pin never named reported %v", err)
	}
}

// CheckRedirect is the whole policy, so it is also checked directly: the asset hosts
// are exactly the two a release download moves to.
func TestCheckRedirectNamesTheAssetHosts(t *testing.T) {
	via := []*http.Request{{URL: mustURL(t, "https://github.com/o/r/releases/download/v1/x.tgz")}}
	allowed := []string{
		"https://release-assets.githubusercontent.com/x",
		"https://objects.githubusercontent.com/x",
	}
	for _, target := range allowed {
		if err := CheckRedirect(&http.Request{URL: mustURL(t, target)}, via); err != nil {
			t.Errorf("%s must be followed: %s", target, err)
		}
	}
	for _, target := range []string{
		"https://downloads.claude.ai.evil.example/x",
		"https://evil.example/x",
		"https://github.com/other/x",
	} {
		if err := CheckRedirect(&http.Request{URL: mustURL(t, target)}, via); err == nil {
			t.Errorf("%s must be refused", target)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s: %s", raw, err)
	}
	return parsed
}
