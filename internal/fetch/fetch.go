// Package fetch is the one HTTP primitive the Go programs use.
//
// Every download a build makes goes through it, so the size ceiling, the status
// handling and the retry policy are decided once. `file:` is supported because
// the offline suites drive the real installers from a local release tree; a
// committed pin may still only name an upstream https origin, which is the gate's
// decision, not this package's.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
)

// Refusal is a download this repository will not act on: a failed status, an
// unreachable host or a payload past its ceiling. It keeps the cause so the retry
// policy can tell a transport blip from a status the pin itself cannot produce.
type Refusal struct {
	Message string
	Cause   error
}

func (r *Refusal) Error() string { return r.Message }

// Unwrap exposes the cause to errors.As.
func (r *Refusal) Unwrap() error { return r.Cause }

func refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

func refuseCause(cause error, format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...), Cause: cause}
}

// Timeout bounds one artifact-sized response.
const Timeout = 300 * time.Second

// Attempts is the total number of tries for a transport failure or a 5xx. An
// HTTP status the pin itself cannot produce (404, 403) is not retried: it is a
// refusal, not a blip.
const Attempts = 6

// Client fetches with a bounded retry policy.
type Client struct {
	HTTP  *http.Client
	Retry failsafe.Executor[[]byte]
	// Log records each fetch once, before the first attempt, for the suites that
	// prove the ordering of a build's network calls.
	Log func(url string)
}

// New returns a client with the reviewed policy.
func New() *Client {
	policy := retrypolicy.NewBuilder[[]byte]().
		WithMaxAttempts(Attempts).
		WithBackoff(50*time.Millisecond, time.Second).
		HandleIf(func(_ []byte, err error) bool {
			var status *statusError
			if errors.As(err, &status) {
				return status.Retryable()
			}
			// A transport failure (DNS, connect, TLS, a truncated body) is the
			// other thing a retry can fix.
			return err != nil
		}).
		Build()
	return &Client{HTTP: &http.Client{Timeout: Timeout}, Retry: failsafe.With(policy)}
}

// statusError carries one HTTP status the caller must not retry blindly.
type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.status) }

// Retryable reports whether waiting could change this status.
func (e *statusError) Retryable() bool { return e.status >= 500 }

// Get fetches url, refusing a payload larger than limit.
func (c *Client) Get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, refuse("cannot fetch %s: %s", rawURL, err)
	}
	if c.Log != nil {
		c.Log(rawURL)
	}
	if parsed.Scheme == "file" {
		return readFile(parsed, rawURL, limit)
	}
	raw, err := c.Retry.GetWithExecution(func(_ failsafe.Execution[[]byte]) ([]byte, error) {
		return c.get(ctx, rawURL, parsed, limit)
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) get(ctx context.Context, rawURL string, parsed *url.URL,
	limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, refuse("cannot fetch %s: %s", rawURL, err)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, refuseCause(err, "cannot fetch %s: %s", rawURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		status := &statusError{status: response.StatusCode}
		return nil, refuseCause(status, "cannot fetch %s: %s", rawURL, status)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, refuseCause(err, "cannot fetch %s: %s", rawURL, err)
	}
	if int64(len(raw)) > limit {
		return nil, refuse("%s is larger than the %d byte ceiling", rawURL, limit)
	}
	return raw, nil
}

func readFile(parsed *url.URL, rawURL string, limit int64) ([]byte, error) {
	if parsed.Host != "" && parsed.Host != "localhost" {
		return nil, refuse("cannot fetch %s: not a plain file url", rawURL)
	}
	path := parsed.Path
	if path == "" {
		path = parsed.Opaque
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, refuse("cannot fetch %s: %s", rawURL, err)
	}
	defer func() { _ = handle.Close() }()
	raw, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil {
		return nil, refuse("cannot fetch %s: %s", rawURL, err)
	}
	if int64(len(raw)) > limit {
		return nil, refuse("%s is larger than the %d byte ceiling", rawURL, limit)
	}
	return raw, nil
}
