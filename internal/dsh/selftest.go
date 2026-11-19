package dsh

import (
	"os"
	"strings"
)

// SelfTest proves the ownership proof degrades in one direction only: an
// unprovable procfs must fail, never pass silently, and only an explicit non-Linux
// fixture host may record a skip.
//
// The case is a unit property of the driver, which is why it is reachable without a
// payload: a gate that quietly stopped proving listener ownership would still pass
// every end-to-end case on a host that has procfs.
func SelfTest() error {
	directory, err := os.MkdirTemp("", "probe-dsh-self-test-")
	if err != nil {
		return refusal(err.Error())
	}
	defer func() { _ = os.RemoveAll(directory) }()
	for _, unproven := range []struct {
		label string
		probe prober
	}{
		{"production capability", prober{host: hostPlatform(), proc: directory}},
		{"linux host", prober{host: "linux", proc: directory}},
	} {
		production := unproven.label == "production capability"
		_, err := unproven.probe.ownership(4321, 1, production)
		if err == nil || !strings.Contains(err.Error(), "ownership cannot be proven") {
			return refusalf("%s: an unproven listener ownership passed (%v)", unproven.label, err)
		}
	}
	skipped, err := (&prober{host: "darwin", proc: directory}).ownership(4321, 1, false)
	if err != nil {
		return refusalf("fixture host skip: %v", err)
	}
	const reason = "fixture host without procfs: no procfs"
	if skipped.Proven || skipped.Reason != reason {
		return refusalf("fixture host skip recorded %+v, want reason %q", skipped, reason)
	}
	return nil
}
