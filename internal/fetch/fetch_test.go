package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
