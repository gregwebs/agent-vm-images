// Package fetch downloads one pinned URL to one scratch path.
//
// It is a deliberate, measured port of `rust_toolchain.py`'s HttpsTransport
// (slice 3 of #187), not the root module's internal/fetch: an example layer is a
// self-contained Go module (ADR 0017) and cannot import it. The differences are
// policy, and each one is stated where it lives:
//
//   - one attempt, no retry (the pin names exact bytes; a retry would break the
//     "no URL fetched twice" oracle);
//   - no redirect is followed (the source reader refuses any 3xx as an
//     unexpected status);
//   - HTTPS only in the shipped client, no `file://` transport;
//   - Proxy: nil and DisableCompression: true, matching Python's http.client.
//     Honouring HTTP_PROXY/HTTPS_PROXY would be a behaviour change dressed as a
//     faithful port, and it would contradict the rule that this binary reads no
//     environment variable of its own (Q5, #231).
//
// The whole-request deadline (300 s) covers connect, TLS, headers and every body
// byte; the inactivity bound (30 s) caps each read. Go's http.Client.Timeout
// alone does not bound inactivity, so the transport wraps its connections.
package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Reviewed bounds. A measured review, not an automatic increase, changes these.
const (
	RequestTimeout    = 300 * time.Second
	InactivityTimeout = 30 * time.Second
	ReadChunkBytes    = 1 << 16
)

// Client is one fetcher. The zero value is not usable; call NewClient.
type Client struct {
	// RequestTimeout bounds the whole request, including the body.
	RequestTimeout time.Duration
	// InactivityTimeout caps one connect, handshake, header read or body read.
	InactivityTimeout time.Duration
	// RootCAs overrides the system pool. It exists for the suite's TLS peer;
	// production leaves it nil and uses the system roots.
	RootCAs *x509.CertPool
	// rewrite is the fixture seam's exact-URL rewriter. It is unexported and
	// never set by a shipped build; the type is built only from NewClient there.
	rewrite func(*http.Request) error
}

// NewClient is the shipped client: HTTPS only, system roots, no proxy, no
// transparent decompression.
func NewClient() *Client {
	return &Client{
		RequestTimeout:    RequestTimeout,
		InactivityTimeout: InactivityTimeout,
	}
}

func (c *Client) requestTimeout() time.Duration {
	if c.RequestTimeout <= 0 {
		return RequestTimeout
	}
	return c.RequestTimeout
}

func (c *Client) inactivityTimeout() time.Duration {
	if c.InactivityTimeout <= 0 {
		return InactivityTimeout
	}
	return c.InactivityTimeout
}

// Fetch downloads url to destination, atomically: it writes destination+".partial"
// exclusively, renames only after status 200, the size cap and Content-Length all
// held, and removes the partial file on every error path.
func (c *Client) Fetch(ctx context.Context, url string, destination string, maxBytes int64) error {
	text := url
	if !strings.HasPrefix(text, "https://") {
		return fmt.Errorf("fetch %s: unsupported URL", text)
	}
	deadline := time.Now().Add(c.requestTimeout())
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	partial := destination + ".partial"
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fetch %s: %v", text, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, text, nil)
	if err != nil {
		return fmt.Errorf("fetch %s: %v", text, err)
	}
	request.Header.Set("Connection", "close")
	request.Header.Set("Accept-Encoding", "identity")
	if c.rewrite != nil {
		if err := c.rewrite(request); err != nil {
			return fmt.Errorf("fetch %s: %v", text, err)
		}
	}
	client := &http.Client{
		Transport: c.transport(deadline),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect is a different object than the pin names. Returning
			// ErrUseLastResponse hands the 3xx to the status check below.
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return fmt.Errorf("fetch %s: download deadline exceeded", text)
		}
		return fmt.Errorf("fetch %s: connection failed: %v", text, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: unexpected HTTP status %d", text, response.StatusCode)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("fetch %s: download deadline exceeded", text)
	}

	handle, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("fetch %s: %v", text, err)
	}
	written, copyErr := writeBounded(ctx, handle, response.Body, maxBytes, text, deadline)
	closeErr := handle.Close()
	if copyErr != nil {
		remove(partial)
		return copyErr
	}
	if closeErr != nil {
		remove(partial)
		return fmt.Errorf("fetch %s: %v", text, closeErr)
	}
	if response.ContentLength >= 0 && response.ContentLength != written {
		remove(partial)
		return fmt.Errorf("fetch %s: truncated stream (%d of %d bytes)", text, written,
			response.ContentLength)
	}
	if err := os.Rename(partial, destination); err != nil {
		remove(partial)
		return fmt.Errorf("fetch %s: %v", text, err)
	}
	return nil
}

func remove(path string) { _ = os.Remove(path) }

// writeBounded copies the body and refuses a stream past maxBytes. The deadline
// is checked between reads, and the connection's own read deadline bounds a
// stalled peer.
func writeBounded(ctx context.Context, handle io.Writer, body io.Reader, maxBytes int64,
	text string, deadline time.Time) (int64, error) {
	buffer := make([]byte, ReadChunkBytes)
	var written int64
	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return written, fmt.Errorf("fetch %s: download deadline exceeded", text)
		}
		count, readErr := body.Read(buffer)
		if count > 0 {
			written += int64(count)
			if written > maxBytes {
				return written, fmt.Errorf("fetch %s: response exceeds %d bytes", text, maxBytes)
			}
			if _, err := handle.Write(buffer[:count]); err != nil {
				return written, fmt.Errorf("fetch %s: read failed or exceeded the deadline: %v",
					text, err)
			}
		}
		if readErr == io.EOF || errors.Is(readErr, io.ErrUnexpectedEOF) {
			// A short body ends the stream from the reader's point of view; the
			// Content-Length comparison below is what names it truncated,
			// exactly as the source reader's `read() == b""` does.
			return written, nil
		}
		if readErr != nil {
			if ctx.Err() != nil || isTimeout(readErr) {
				return written, fmt.Errorf("fetch %s: download deadline exceeded", text)
			}
			return written, fmt.Errorf("fetch %s: read failed or exceeded the deadline: %v",
				text, readErr)
		}
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// transport builds the one transport a Fetch uses. Nothing ambient is consulted:
// Proxy is nil (not ProxyFromEnvironment), compression and keep-alives are off,
// and every connection gets the deadline reader.
func (c *Client) transport(deadline time.Time) *http.Transport {
	dialer := &net.Dialer{Timeout: c.inactivityTimeout()}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.RootCAs != nil {
		tlsConfig.RootCAs = c.RootCAs
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           c.dialContext(dialer, deadline),
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   c.inactivityTimeout(),
		ResponseHeaderTimeout: c.inactivityTimeout(),
		ExpectContinueTimeout: c.inactivityTimeout(),
		DisableCompression:    true,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
	}
	return transport
}

func (c *Client) dialContext(dialer *net.Dialer, deadline time.Time) func(
	context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &deadlineConn{Conn: conn, deadline: deadline,
			inactivity: c.inactivityTimeout()}, nil
	}
}

// deadlineConn caps every read to min(remaining request budget, inactivity
// bound). Go's http.Client.Timeout covers the whole request by cancelling it, but
// a peer that trickles just above an inactivity bound needs the socket deadline.
type deadlineConn struct {
	net.Conn
	deadline   time.Time
	inactivity time.Duration
}

func (c *deadlineConn) Read(buffer []byte) (int, error) {
	remaining := time.Until(c.deadline)
	if remaining <= 0 {
		return 0, os.ErrDeadlineExceeded
	}
	limit := c.inactivity
	if remaining < limit {
		limit = remaining
	}
	if err := c.SetReadDeadline(time.Now().Add(limit)); err != nil {
		return 0, err
	}
	return c.Conn.Read(buffer)
}
