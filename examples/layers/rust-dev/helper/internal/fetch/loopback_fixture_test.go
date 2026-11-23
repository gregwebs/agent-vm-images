//go:build example_fixture

package fetch

// The loopback rewriter's own two halves (Q1 of #231): it rewrites the exact
// canonical URLs it was given, and it refuses any other URL -- including a
// different path on the pinned host -- before a connection is made. The listener
// is this test's, never the binary's.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoopbackRewritesOnlyExactPinnedURLs(t *testing.T) {
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter,
		request *http.Request) {
		requests <- request.URL.Path
		fmt.Fprint(writer, "payload")
	}))
	defer server.Close()
	pinned := []string{"https://static.rust-lang.org/dist/2026-01-01/cargo.tar.xz"}
	client, err := NewLoopbackClient(server.URL, pinned)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "pinned")
	if err := client.Fetch(context.Background(), pinned[0], destination, 1<<20); err != nil {
		t.Fatalf("the pinned URL was refused: %v", err)
	}
	if path := <-requests; path != "/dist/2026-01-01/cargo.tar.xz" {
		t.Fatalf("server saw %q", path)
	}
	// Same host, different path: the pin does not name it, so it is refused and
	// the listener never sees a request.
	err = client.Fetch(context.Background(), "https://static.rust-lang.org/dist/other.tar.xz",
		filepath.Join(t.TempDir(), "other"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "not a URL the pin names") {
		t.Fatalf("a non-pinned URL was not refused: %v", err)
	}
	select {
	case path := <-requests:
		t.Fatalf("a non-pinned URL reached the listener: %q", path)
	default:
	}
}

func TestLoopbackOriginIsStrict(t *testing.T) {
	for _, origin := range []string{
		"https://127.0.0.1:1234", "http://localhost:1234", "http://127.0.0.1",
		"http://127.0.0.1:1234/path", "http://user@127.0.0.1:1234", "http://10.0.0.1:1234",
	} {
		if _, err := NewLoopbackClient(origin, []string{"https://static.rust-lang.org/x"}); err == nil {
			t.Errorf("origin %q was accepted", origin)
		}
	}
}
