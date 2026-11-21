// Package kacheprobe decides a `kache doctor --probe --json` report.
//
// `kache doctor` reports findings and still exits 0, so its exit status alone
// proves nothing: a broken store, wrapper or probe is a line of output. This
// reads the versioned JSON document instead and refuses a report that says
// `success` is false, that a non-optional check failed, that the
// compile/store/restore probe did not pass, that the C/C++ shims are not active,
// or that the store sits on storage its index cannot live on. A daemon that has
// not been started yet is the one finding the image does not install for itself,
// so it stays optional.
//
// This is a port of the layer's former `kache_probe.py` (#187, slice 1). It keeps
// that program's decisions, its order of findings and its diagnostics, including
// the tolerance it inherited from #156: a report is read for truthiness the way
// Python reads it, so `"pass": 1` and `"pass": "false"` both count as passing.
// Tightening that is #156's, not this port's.
package kacheprobe

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Prefix is the one diagnostic prefix every refusal carries.
const Prefix = "verify-toolchain"

// The labels kache puts on the three findings this program requires. `doctor`
// grades the shims as optional because a Rust-only setup may skip them, but this
// image installs them and puts them first on PATH, so a report that says
// otherwise means the shims stopped being active.
const (
	probeLabel   = "Cache probe"
	shimsLabel   = "C/C++ shims"
	storeFSLabel = "Cache FS"
)

// Decide reads the report at path and returns the status: 0 when every required
// finding holds, 1 otherwise, with one line per reason on stderr. It never
// returns another status, and it writes nothing to stdout.
func Decide(path string, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return refuse(stderr, err.Error())
	}
	report, err := decode(raw)
	if err != nil {
		return refuse(stderr, err.Error())
	}
	members, ok := report.(map[string]any)
	if !ok {
		return refuse(stderr, "the kache doctor report is not a JSON object")
	}
	results, ok := members["checks"].([]any)
	if !ok {
		return refuse(stderr, "the kache doctor report carries no checks")
	}
	checks := make([]map[string]any, 0, len(results))
	for _, result := range results {
		check, ok := result.(map[string]any)
		if !ok {
			return refuse(stderr, "the kache doctor report carries a check that is not a JSON object")
		}
		checks = append(checks, check)
	}
	status := 0
	if !truthy(members["success"]) {
		status = refuse(stderr, "kache doctor reported failure")
	}
	for _, check := range checks {
		if truthy(check["pass"]) || truthy(check["optional"]) {
			continue
		}
		// A failed write to the caller's stderr cannot be reported anywhere else:
		// the status below already says the report was refused.
		_, _ = fmt.Fprintf(stderr, "%s: kache doctor failed: %s: %s\n",
			Prefix, pyString(check["label"]), pyString(check["detail"]))
		status = 1
	}
	// `doctor` computes the compile, store and restore cycle once. The image's
	// whole reason for running kache depends on it passing, so a report that
	// carries no such finding is as wrong as one that carries a failing one.
	probes := withLabel(checks, probeLabel)
	if len(probes) != 1 {
		status = refuse(stderr, fmt.Sprintf("expected exactly one %s check, found %d", probeLabel, len(probes)))
	} else if !truthy(probes[0]["pass"]) {
		status = refuse(stderr, "the kache compile/store/restore probe did not pass: "+pyString(probes[0]["detail"]))
	}
	shims := withLabel(checks, shimsLabel)
	if len(shims) != 1 {
		status = refuse(stderr, fmt.Sprintf("expected exactly one %s check, found %d", shimsLabel, len(shims)))
	} else if !truthy(shims[0]["pass"]) {
		status = refuse(stderr, "the C/C++ shims are not active: "+pyString(shims[0]["detail"]))
	}
	// The store is a WAL-mode SQLite index, so a report that places it on a
	// network or FUSE mount is describing the corruption case. `doctor` already
	// grades this check non-optional, so the loop above fails it too; naming it
	// here keeps the reason readable and keeps the requirement if that grading
	// ever changes.
	storeFS := withLabel(checks, storeFSLabel)
	if len(storeFS) != 1 {
		status = refuse(stderr, fmt.Sprintf("expected exactly one %s check, found %d", storeFSLabel, len(storeFS)))
	} else if !truthy(storeFS[0]["pass"]) {
		status = refuse(stderr, "the cache store is on storage its index cannot live on: "+pyString(storeFS[0]["detail"]))
	}
	return status
}

// refuse writes one prefixed diagnostic and returns the refusal status.
func refuse(stderr io.Writer, message string) int {
	_, _ = fmt.Fprintf(stderr, "%s: %s\n", Prefix, message)
	return 1
}

// decode is the predecessor's `json.load` over committed-to-shape bytes: the
// numbers stay tokens so a label or detail that is a number prints the way Python
// printed it. Go's decoder refuses the NaN and Infinity literals Python accepted;
// kache's `encoding/json` marshal cannot emit them, so there is no report a real
// `kache doctor` can produce that reaches the difference.
func decode(raw []byte) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing bytes after the kache doctor report")
	}
	return value, nil
}

// withLabel selects the findings that carry exactly this label, in report order.
// A label that is not a string matches nothing, as Python's `==` over a string
// did.
func withLabel(checks []map[string]any, label string) []map[string]any {
	var selected []map[string]any
	for _, check := range checks {
		if name, ok := check["label"].(string); ok && name == label {
			selected = append(selected, check)
		}
	}
	return selected
}

// truthy is Python's truth-value test over a decoded JSON value, because the
// predecessor decided with `if not report.get("success")` and
// `check.get("pass") or check.get("optional")`. The port keeps the tolerance it
// inherited (#156): a report saying `"pass": 1` or `"pass": "false"` passes here
// as it passed there.
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		number, err := strconv.ParseFloat(typed.String(), 64)
		return err != nil || number != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// pyString renders a decoded value the way Python's f-string did, so a field that
// is present but not a string still produces the predecessor's line: `None`,
// `True` and numbers rather than Go's `<nil>`, `true` and `1e+06`. A JSON
// container has no Python repr here; it prints as its JSON text, which no report
// from `kache doctor` produces.
func pyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}
