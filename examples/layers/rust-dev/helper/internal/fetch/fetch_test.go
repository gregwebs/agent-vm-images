package fetch

import (
	"bufio"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tlsClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	client := NewClient()
	client.RootCAs = pool
	return client
}

func requireNoFiles(t *testing.T, destination string) {
	t.Helper()
	for _, path := range []string{destination, destination + ".partial"} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("%s survived a failed fetch", path)
		}
	}
}

// TestFetchRefusals is rows 1, 5, 6 and 8: the size cap, status 200 only, the
// Content-Length check, and the atomic staging that leaves neither file behind.
func TestFetchRefusals(t *testing.T) {
	var requests int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter,
		request *http.Request) {
		atomic.AddInt32(&requests, 1)
		switch request.URL.Path {
		case "/ok":
			_, _ = fmt.Fprint(writer, "payload")
		case "/404":
			writer.WriteHeader(http.StatusNotFound)
		case "/500":
			writer.WriteHeader(http.StatusInternalServerError)
		case "/302":
			http.Redirect(writer, request, "/ok", http.StatusFound)
		case "/oversized":
			_, _ = fmt.Fprint(writer, strings.Repeat("x", 64))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := tlsClient(t, server)

	cases := []struct {
		name        string
		path        string
		max         int64
		fragments   []string
		destination string
	}{
		{"status_404", "/404", 1 << 20, []string{"unexpected HTTP status 404"}, "a"},
		{"status_500", "/500", 1 << 20, []string{"unexpected HTTP status 500"}, "b"},
		{"redirect_302", "/302", 1 << 20, []string{"unexpected HTTP status 302"}, "c"},
		{"oversized_stream", "/oversized", 16, []string{"response exceeds 16 bytes"}, "d"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), test.destination)
			err := client.Fetch(context.Background(), server.URL+test.path, destination, test.max)
			if err == nil {
				t.Fatalf("fetch succeeded, want a refusal")
			}
			for _, fragment := range test.fragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("refusal %q lacks %q", err, fragment)
				}
			}
			requireNoFiles(t, destination)
		})
	}

	t.Run("clean_fetch", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "clean")
		if err := client.Fetch(context.Background(), server.URL+"/ok", destination, 1<<20); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		raw, err := os.ReadFile(destination)
		if err != nil || string(raw) != "payload" {
			t.Fatalf("destination is %q, %v", raw, err)
		}
		if _, err := os.Lstat(destination + ".partial"); err == nil {
			t.Fatal("the partial file survived a successful fetch")
		}
	})
}

// TestNoRetryOnServerError is row 4: exactly one request, even for a 500.
func TestNoRetryOnServerError(t *testing.T) {
	var requests int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter,
		request *http.Request) {
		atomic.AddInt32(&requests, 1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := tlsClient(t, server)
	destination := filepath.Join(t.TempDir(), "retry")
	err := client.Fetch(context.Background(), server.URL+"/x", destination, 1<<20)
	if err == nil {
		t.Fatal("fetch succeeded")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("the transport made %d requests, want exactly 1", got)
	}
}

// TestTransportPolicy is row 7 and Q5: Proxy is nil (an ambient HTTPS_PROXY is
// ignored), the request says Accept-Encoding: identity and Connection: close, and
// a non-HTTPS URL is refused before any connection.
func TestTransportPolicy(t *testing.T) {
	var seen http.Header
	var proxied int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&proxied, 1)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter,
		request *http.Request) {
		seen = request.Header.Clone()
		_, _ = fmt.Fprint(writer, "payload")
	}))
	defer server.Close()
	client := tlsClient(t, server)
	destination := filepath.Join(t.TempDir(), "policy")
	if err := client.Fetch(context.Background(), server.URL+"/x", destination, 1<<20); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := atomic.LoadInt32(&proxied); got != 0 {
		t.Fatalf("the fetch consulted the ambient proxy %d times", got)
	}
	if got := seen.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding is %q, want identity", got)
	}
	if got := seen.Get("Connection"); got != "close" {
		t.Errorf("Connection is %q, want close", got)
	}
	if err := client.Fetch(context.Background(), "http://127.0.0.1:1/x", destination, 1<<20); err == nil ||
		!strings.Contains(err.Error(), "unsupported URL") {
		t.Fatalf("a plain-HTTP URL was not refused: %v", err)
	}
}

// TestFetchPartialCleanup is row 8 on the truncated and deadline paths.
func TestFetchPartialCleanup(t *testing.T) {
	t.Run("truncated_content_length", func(t *testing.T) {
		address, closePeer := rawPeer(t, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc", false)
		defer closePeer()
		client := NewClient()
		client.rewrite = loopbackRewrite(address)
		destination := filepath.Join(t.TempDir(), "truncated")
		err := client.Fetch(context.Background(), "https://static.rust-lang.org/x", destination, 1<<20)
		if err == nil || !strings.Contains(err.Error(), "truncated stream (3 of 10 bytes)") {
			t.Fatalf("refusal is %v, want a truncated stream", err)
		}
		requireNoFiles(t, destination)
	})
}

// TestRealLoopbackDeadlines is rows 2 and 3: a peer that trickles bytes below the
// inactivity bound is refused by the deadline, not waited out.
func TestRealLoopbackDeadlines(t *testing.T) {
	t.Run("trickle_headers", func(t *testing.T) {
		address, closePeer := rawPeer(t, "", true)
		defer closePeer()
		client := NewClient()
		client.RequestTimeout = 5 * time.Second
		client.InactivityTimeout = 150 * time.Millisecond
		client.rewrite = loopbackRewrite(address)
		destination := filepath.Join(t.TempDir(), "headers")
		started := time.Now()
		err := client.Fetch(context.Background(), "https://static.rust-lang.org/x", destination, 1<<20)
		if err == nil {
			t.Fatal("a stalled header read succeeded")
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("the stalled peer held the fetch for %s", elapsed)
		}
		requireNoFiles(t, destination)
	})
	t.Run("stalled_body_inactivity", func(t *testing.T) {
		address, closePeer := rawPeer(t,
			"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nfirst", true)
		defer closePeer()
		client := NewClient()
		client.RequestTimeout = 5 * time.Second
		client.InactivityTimeout = 150 * time.Millisecond
		client.rewrite = loopbackRewrite(address)
		destination := filepath.Join(t.TempDir(), "body")
		err := client.Fetch(context.Background(), "https://static.rust-lang.org/x", destination, 1<<20)
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("refusal is %v, want a deadline", err)
		}
		requireNoFiles(t, destination)
	})
}

// rawPeer starts a TCP listener that writes reply, then holds the connection open
// without another byte. The returned address is host:port.
func rawPeer(t *testing.T, reply string, hold bool) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			close(done)
			return
		}
		defer func() { _ = connection.Close() }()
		// Consume the request headers before answering, so closing is a clean
		// FIN rather than a reset from unread data.
		reader := bufio.NewReader(connection)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(connection, reply)
		if !hold {
			return
		}
		<-done
	}()
	return listener.Addr().String(), func() {
		close(done)
		_ = listener.Close()
	}
}

func loopbackRewrite(address string) func(*http.Request) error {
	return func(request *http.Request) error {
		request.URL.Scheme = "http"
		request.URL.Host = address
		request.Host = address
		return nil
	}
}
