package kacheprobe

// These cases are coverage, not the oracle: the oracle for this program is the
// rust-dev native verifier leg and the layer-build rows, which decide reports
// `kache doctor` actually produced on a real host (#187). What they hold here is
// the pair of properties the harness cannot see cheaply -- the exact diagnostics
// the shell suite greps for, and the inherited truthiness tolerance #156 owns.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// check is one finding in a `kache doctor --probe --json` report. The report is
// built from these rather than spliced as text, so a case that removes or changes
// one finding cannot accidentally produce invalid JSON instead.
type check struct {
	label    string
	pass     string
	optional string
	detail   string
}

func finding(label, pass string) check {
	return check{label: label, pass: pass, detail: "detail of " + label}
}

func (c check) json() string {
	members := []string{`"label": ` + quote(c.label), `"pass": ` + c.pass}
	if c.optional != "" {
		members = append(members, `"optional": `+c.optional)
	}
	if c.detail != "" {
		members = append(members, `"detail": `+quote(c.detail))
	}
	return "{" + strings.Join(members, ", ") + "}"
}

func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// findings is a passing report: every required finding holds and the one finding
// the image deliberately does not install for itself (a started daemon) is
// optional.
func findings() []check {
	return []check{
		{label: "Version", pass: "true", detail: "kache 2.0.0"},
		finding("Cache FS", "true"),
		finding("C/C++ shims", "true"),
		{label: "Daemon", pass: "false", optional: "true", detail: "not started"},
		finding("Cache probe", "true"),
	}
}

func report(success string, checks []check) string {
	var encoded []string
	for _, one := range checks {
		encoded = append(encoded, one.json())
	}
	return `{"version": 1, "success": ` + success + `, "checks": [` +
		strings.Join(encoded, ", ") + `]}`
}

// replace swaps the finding with this label, or removes it when the replacement is
// nil.
func replace(checks []check, label string, replacement *check) []check {
	var updated []check
	for _, one := range checks {
		if one.label != label {
			updated = append(updated, one)
			continue
		}
		if replacement != nil {
			updated = append(updated, *replacement)
		}
	}
	return updated
}

func decide(t *testing.T, report string) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doctor.json")
	if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := Decide(path, &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Fatalf("Decide wrote to stdout: %q", stdout.String())
	}
	return status, stderr.String()
}

func TestDecide(t *testing.T) {
	failedProbe := check{label: "Cache probe", pass: "false", detail: "stored but not restored"}
	failedShims := check{label: "C/C++ shims", pass: "false", detail: "no shim on PATH"}
	failedStore := check{label: "Cache FS", pass: "false", detail: "/mnt/nfs/kache"}
	cases := []struct {
		name   string
		report string
		status int
		stderr string
	}{{
		name:   "a passing report is silent",
		report: report("true", findings()),
		status: 0,
	}, {
		name: "an optional finding may fail",
		report: report("true", append(findings(),
			check{label: "Unused", pass: "false", optional: "true", detail: "d"})),
		status: 0,
	}, {
		name:   "success false is a refusal",
		report: report("false", findings()),
		status: 1,
		stderr: "verify-toolchain: kache doctor reported failure\n",
	}, {
		name:   "a non-optional failure names its finding",
		report: report("true", replace(findings(), "Cache FS", &failedStore)),
		status: 1,
		stderr: "verify-toolchain: kache doctor failed: Cache FS: /mnt/nfs/kache\n" +
			"verify-toolchain: the cache store is on storage its index cannot live on: /mnt/nfs/kache\n",
	}, {
		name:   "the probe must be exactly one finding",
		report: report("true", replace(findings(), "Cache probe", nil)),
		status: 1,
		stderr: "verify-toolchain: expected exactly one Cache probe check, found 0\n",
	}, {
		name:   "the probe must pass",
		report: report("true", replace(findings(), "Cache probe", &failedProbe)),
		status: 1,
		stderr: "verify-toolchain: kache doctor failed: Cache probe: stored but not restored\n" +
			"verify-toolchain: the kache compile/store/restore probe did not pass: stored but not restored\n",
	}, {
		name:   "the C/C++ shims must be active",
		report: report("true", replace(findings(), "C/C++ shims", &failedShims)),
		status: 1,
		stderr: "verify-toolchain: kache doctor failed: C/C++ shims: no shim on PATH\n" +
			"verify-toolchain: the C/C++ shims are not active: no shim on PATH\n",
	}, {
		name:   "the shims finding must be present",
		report: report("true", replace(findings(), "C/C++ shims", nil)),
		status: 1,
		stderr: "verify-toolchain: expected exactly one C/C++ shims check, found 0\n",
	}, {
		name:   "the store finding must be present",
		report: report("true", replace(findings(), "Cache FS", nil)),
		status: 1,
		stderr: "verify-toolchain: expected exactly one Cache FS check, found 0\n",
	}, {
		name:   "a report that is not an object",
		report: `[]`,
		status: 1,
		stderr: "verify-toolchain: the kache doctor report is not a JSON object\n",
	}, {
		name:   "a report with no checks",
		report: `{"success": true}`,
		status: 1,
		stderr: "verify-toolchain: the kache doctor report carries no checks\n",
	}, {
		name:   "a check that is not an object",
		report: `{"success": true, "checks": [1]}`,
		status: 1,
		stderr: "verify-toolchain: the kache doctor report carries a check that is not a JSON object\n",
	}, {
		name:   "invalid JSON",
		report: `{`,
		status: 1,
		stderr: "verify-toolchain: unexpected EOF\n",
	}, {
		// #156 owns the tightening: this port keeps the tolerance it inherited, so
		// a report that says `1` passes exactly as the Python script let it.
		name: "truthiness is Python's, not JSON's",
		report: report("true", append(findings(),
			check{label: "Extra", pass: "1", detail: "truthy but not true"})),
		status: 0,
	}, {
		name:   "a non-empty string passes a truthiness test",
		report: report("true", replace(findings(), "C/C++ shims", &check{label: "C/C++ shims", pass: `"false"`, detail: "tolerated by #156"})),
		status: 0,
	}, {
		// A field that is present but not a string still prints the way Python's
		// f-string printed it, so a log reader sees the same line.
		name:   "a missing detail prints as None",
		report: report("true", replace(findings(), "Cache FS", &check{label: "Cache FS", pass: "false"})),
		status: 1,
		stderr: "verify-toolchain: kache doctor failed: Cache FS: None\n" +
			"verify-toolchain: the cache store is on storage its index cannot live on: None\n",
	}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, stderr := decide(t, test.report)
			if status != test.status {
				t.Fatalf("status %d, want %d (stderr: %s)", status, test.status, stderr)
			}
			if stderr != test.stderr {
				t.Fatalf("stderr:\n%q\nwant:\n%q", stderr, test.stderr)
			}
		})
	}
}

// The layer's harness greps this file for the requirement, so the string is part
// of the contract twice over: the diagnostic and the source that holds it.
func TestShimsRequirementIsNamed(t *testing.T) {
	source, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(source, []byte("C/C++ shims")) {
		t.Fatal("probe.go no longer names the C/C++ shims requirement the layer greps for")
	}
}

func TestDecideWithoutAReport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := Decide(filepath.Join(t.TempDir(), "absent.json"), &stdout, &stderr)
	if status != 1 {
		t.Fatalf("status %d, want 1", status)
	}
	if !strings.HasPrefix(stderr.String(), "verify-toolchain: ") {
		t.Fatalf("an unreadable report did not carry the prefix: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("Decide wrote to stdout: %q", stdout.String())
	}
}
