//go:build example_fixture

package fetch

// The fixture build's loopback rewriter. It is the ONE change a suite gets to the
// real Fetch path: it rewrites the exact canonical URLs the pin names onto a
// listener the harness owns. The shipped build has no rewriter and no HTTPS
// exception; this file is not compiled into it.
//
// Q1 of #231 binds two things this file must not lose:
//
//   - the match is the exact canonical URL, not a host and not a prefix. A URL
//     the pin does not name -- even on static.rust-lang.org -- is refused, so a
//     fixture binary cannot be used to fetch anything the pin did not select;
//   - the listener is in the harness, not here. This file contains no net.Listen
//     and no server; it only points a request at the caller's origin.

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// NewLoopbackClient returns the production client with an exact-URL rewrite onto
// origin, a strict `http://127.0.0.1:<port>` origin with no path, query,
// fragment or userinfo. pinned is the closed set the pin's closure names.
func NewLoopbackClient(origin string, pinned []string) (*Client, error) {
	parsed, err := parseLoopbackOrigin(origin)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(pinned))
	for _, canonical := range pinned {
		allowed[canonical] = true
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("loopback origin needs at least one pinned URL")
	}
	client := NewClient()
	client.rewrite = func(request *http.Request) error {
		if !allowed[request.URL.String()] {
			return fmt.Errorf("not a URL the pin names")
		}
		request.URL.Scheme = "http"
		request.URL.Host = parsed
		request.Host = parsed
		return nil
	}
	return client, nil
}

// parseLoopbackOrigin accepts only `http://127.0.0.1:<numeric port>`.
func parseLoopbackOrigin(origin string) (string, error) {
	parsed, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("loopback origin: %v", err)
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("loopback origin must be http://127.0.0.1:<port>")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 || !strings.Contains(parsed.Host, ":") {
		return "", fmt.Errorf("loopback origin must name a numeric port")
	}
	return parsed.Host, nil
}
