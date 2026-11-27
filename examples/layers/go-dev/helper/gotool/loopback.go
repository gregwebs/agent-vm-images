package gotool

// The loopback rule for a fixture origin. It lives beside the environment owner
// because two programs need it: the layer's `example_fixture` installer and the
// maintainer stager's fixture build. One spelling of the rule is what keeps them from
// accepting different origins.

import (
	"fmt"
	"net/url"
	"strconv"
)

// RequireLoopback accepts only a suite recorder's strict origin shape: http, a literal
// loopback IP, and a numeric port. An origin that needs a name to resolve is not a
// loopback recorder, and a fixture build is not the place to discover that. flag is
// the caller's own spelling in the diagnostic, so the message names the flag the user
// typed.
func RequireLoopback(flag, origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("%s: %v", flag, err)
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be http://IP:PORT, not %q", flag, origin)
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("%s must name the loopback address, not %q", flag, host)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s must carry a numeric port, not %q", flag, origin)
	}
	return nil
}
