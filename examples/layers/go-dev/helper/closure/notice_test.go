package closure

// The notice-evidence rules, including the two #188 G3 collapses: a duplicate
// framed section and a duplicate notice name inside one module. Both used to
// reach the set comparison as fewer rows than the file carried, so the file
// checked itself out.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frame renders one length-framed notice section the way the writer does.
func frame(id ModuleID, name string, body []byte) []byte {
	digest := sha256.Sum256(body)
	return []byte(fmt.Sprintf("=== %s %s %s bytes=%d sha256=%s\n%s\n", id.Path, id.Version, name,
		len(body), hex.EncodeToString(digest[:]), body))
}

// noticesOf splits the committed evidence into whole frames, so a case can move
// one around without re-deriving its bytes.
func noticesOf(t *testing.T, directory string) ([]byte, [][]byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, string(FileLicenses)))
	if err != nil {
		t.Fatalf("read %s: %v", FileLicenses, err)
	}
	marker := []byte("=== ")
	first := bytes.Index(raw, marker)
	if first < 0 {
		t.Fatalf("%s carries no framed section", FileLicenses)
	}
	header := raw[:first]
	frames := [][]byte{}
	rest := raw[first:]
	for len(rest) > 0 {
		next := bytes.Index(rest[len(marker):], marker)
		if next < 0 {
			frames = append(frames, rest)
			break
		}
		next += len(marker)
		frames = append(frames, rest[:next])
		rest = rest[next:]
	}
	if len(frames) < 30 {
		t.Fatalf("found %d notice frames, want at least 30: the split is wrong", len(frames))
	}
	if string(append(append([]byte{}, header...), joinFrames(frames)...)) != string(raw) {
		t.Fatal("the frame split does not rejoin to the committed bytes")
	}
	return header, frames
}

// TestNoticeFramingRefuses holds the framing parser: the length is a byte count,
// so a truncation or a drift is refused rather than partially accepted.
func TestNoticeFramingRefuses(t *testing.T) {
	directory := copyCommitted(t)
	header, frames := noticesOf(t, directory)
	for _, testCase := range []struct {
		name     string
		fragment string
		content  []byte
	}{
		{
			name:     "the last frame loses its terminator",
			fragment: "section is not newline-terminated",
			content: func() []byte {
				joined := joinFrames(frames)
				return append(append([]byte{}, header...), joined[:len(joined)-1]...)
			}(),
		},
		{
			name:     "a frame header is malformed",
			fragment: "malformed section header:",
			content: append(append([]byte{}, header...),
				[]byte(strings.Replace(string(joinFrames(frames)), "=== ", "== ", 1))...),
		},
		{
			name:     "a frame claims more bytes than it carries",
			fragment: "truncated section for ",
			content: append(append([]byte{}, header...),
				[]byte(strings.Replace(string(joinFrames(frames)), "bytes=1", "bytes=999999", 1))...),
		},
		{
			name:     "a body does not match its digest",
			fragment: "license evidence differs from the committed digest:",
			content: func() []byte {
				// The first frame keeps its header and loses one body byte, so the
				// framing still parses and the digest is the rule that fires.
				first := append([]byte{}, frames[0]...)
				end := bytes.Index(first, []byte("\n")) + 1
				first[end] ^= 0x20
				mutated := append([][]byte{first}, frames[1:]...)
				return append(append([]byte{}, header...), joinFrames(mutated)...)
			}(),
		},
		{
			// The count used to be accumulated into an int and then used as a slice
			// bound, so this panicked (status 2) instead of refusing. It is a
			// truncated section: there are not that many bytes left.
			name:     "a frame count larger than the file",
			fragment: "truncated section for ",
			content: append(append([]byte{}, header...),
				[]byte(strings.Replace(string(joinFrames(frames)), "bytes=1",
					"bytes=18446744073709551615", 1))...),
		},
		{
			name:     "the header is not the reviewed one",
			fragment: "unexpected header",
			content:  []byte("# Third-party license files for somewhere else\n"),
		},
		{
			name:     "the file carries no frames",
			fragment: "no license sections",
			content:  header,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseLicenses(testCase.content, string(FileLicenses))
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("got %v, wanted %q", err, testCase.fragment)
			}
		})
	}
}

func joinFrames(frames [][]byte) []byte {
	joined := []byte{}
	for _, frame := range frames {
		joined = append(joined, frame...)
	}
	return joined
}

// TestNoticeIdentityRefuses is the loader-level half: what the pin records must be
// exactly what the file frames, and a path-keyed map may not absorb a duplicate.
func TestNoticeIdentityRefuses(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		fragment string
		apply    func(t *testing.T, directory string)
	}{
		{
			name:     "a duplicated framed section",
			fragment: "duplicate notice section for ",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				_, frames := noticesOf(t, directory)
				appendTo2(t, directory, FileLicenses, frames[0])
			},
		},
		{
			name:     "a framed section the pin does not record",
			fragment: "license evidence does not match pin.json for",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				id := ModuleID{Path: "github.com/fatih/camelcase", Version: "v1.0.0"}
				appendTo2(t, directory, FileLicenses,
					frame(id, "LICENSE.extra", []byte("unreviewed notice bytes\n")))
			},
		},
		{
			name:     "a recorded notice with no frame",
			fragment: "license evidence does not match pin.json for",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					for _, raw := range document["modules"].([]any) {
						module := raw.(map[string]any)
						if module["path"] != "github.com/fatih/camelcase" {
							continue
						}
						module["licenses"] = append(module["licenses"].([]any), map[string]any{
							"name":   "LICENSE.missing",
							"sha256": strings.Repeat("1", 64),
						})
					}
				})
			},
		},
		{
			name:     "a duplicate notice name inside one module",
			fragment: "pin.json: duplicate notice ",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					for _, raw := range document["modules"].([]any) {
						module := raw.(map[string]any)
						if module["path"] != "github.com/fatih/camelcase" {
							continue
						}
						licenses := module["licenses"].([]any)
						module["licenses"] = append(licenses, licenses[0])
					}
				})
			},
		},
		{
			name:     "an unreviewed nested notice layout",
			fragment: "unsupported license layout",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				id := ModuleID{Path: "github.com/fatih/camelcase", Version: "v1.0.0"}
				body := []byte("nested notice bytes\n")
				digest := sha256.Sum256(body)
				appendTo2(t, directory, FileLicenses, frame(id, "sub/LICENSE", body))
				editPin(t, directory, func(document map[string]any) {
					for _, raw := range document["modules"].([]any) {
						module := raw.(map[string]any)
						if module["path"] != "github.com/fatih/camelcase" {
							continue
						}
						module["licenses"] = append(module["licenses"].([]any), map[string]any{
							"name":   "sub/LICENSE",
							"sha256": hex.EncodeToString(digest[:]),
						})
					}
				})
			},
		},
		{
			// The regression for the overflow: a coherent, re-pinned tree whose first
			// frame carries a 20-digit byte count. The refusal is the truncated
			// section, not a panic and not a digest disagreement.
			name:     "a frame count larger than the file, re-pinned",
			fragment: "truncated section for ",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileLicenses, replaceFirstSize)
			},
		},
		{
			name:     "the header names another release",
			fragment: "license evidence header disagrees with the pinned release",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileLicenses, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), "# Third-party license files for "+
						Gopls+" v0.23.0", "# Third-party license files for "+Gopls+" v0.22.0", 1))
				})
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyCommitted(t)
			testCase.apply(t, directory)
			repin(t, directory)
			requireRefusal(t, directory, testCase.fragment)
		})
	}
}

// replaceFirstSize swaps the first framed byte count for a 20-digit one, leaving
// every other byte alone: the frame then claims more bytes than the file has.
func replaceFirstSize(raw []byte) []byte {
	text := string(raw)
	start := strings.Index(text, "bytes=")
	if start < 0 {
		return raw
	}
	end := start + len("bytes=")
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	return []byte(text[:start] + "bytes=18446744073709551615" + text[end:])
}

// appendTo2 appends raw bytes to an input.
func appendTo2(t *testing.T, directory string, name Filename, content []byte) {
	t.Helper()
	rewrite(t, directory, name, func(raw []byte) []byte {
		return append(raw, content...)
	})
}
