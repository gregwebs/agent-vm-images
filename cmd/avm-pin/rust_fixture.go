//go:build example_fixture

package main

// This file exists only in a `-tags example_fixture` build, and it is the only
// way a suite-only input reaches `avm-pin rust-toolchain`. The tag protects the
// seam three ways at once:
//
//   - it is a compile-time boundary, so the code that reads a fixture byte store
//     is not in the shipped binary;
//   - nothing in CI or in a maintainer's wrapper passes the tag, so a pin cannot
//     be staged from fixture bytes by accident; and
//   - `rust_toolchain_seams_test.go` builds both variants and holds the shipped
//     one to carrying none of the seam names, and the fixture one to answering
//     `--fixture-seams`.
//
// The store is the same `dist.json` the Python fixture writes
// (`script/test/fixtures/rust-toolchain/fixture.py`), so the differential drives
// both writers over literally the same bytes.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/toolchain"
)

// rustToolchainSeams names the suite-only inputs this build honours. The shipped
// build carries these names nowhere; the seam test holds both halves.
const (
	fixtureDistName  = "FIXTURE_DIST"
	fixtureFaultName = "FIXTURE_FAULT"
)

type rustToolchainSeams struct {
	Requested bool
	Names     []string
}

// rustToolchainSeamNames is this build's closed seam list.
var rustToolchainSeamNames = []string{fixtureDistName, fixtureFaultName}

type rustToolchainFixtureArguments struct {
	FixtureSeams bool `arg:"--fixture-seams" help:"print the suite-only inputs this build honours; writes nothing"`
}

func (fixture rustToolchainFixtureArguments) seams() (rustToolchainSeams, error) {
	if !fixture.FixtureSeams {
		return rustToolchainSeams{}, nil
	}
	return rustToolchainSeams{Requested: true,
		Names: append([]string{}, rustToolchainSeamNames...)}, nil
}

// trust is the fixture's throwaway primary, read from the same dist.json that
// names its key: the pin the writer stages is held to the key that signed it.
func (fixture rustToolchainFixtureArguments) trust() (toolchain.TrustPolicy, error) {
	dist, err := loadFixtureDist()
	if err != nil {
		return toolchain.TrustPolicy{}, err
	}
	fingerprint, err := toolchain.ParseFingerprint(dist.Fingerprint, "fixture fingerprint")
	if err != nil {
		return toolchain.TrustPolicy{}, err
	}
	return toolchain.TrustRoot(fingerprint, []toolchain.Fingerprint{fingerprint}), nil
}

// transport is the fixture byte store, with the suite's named faults applied to
// the store and never to the code under test.
func (fixture rustToolchainFixtureArguments) transport() (toolchain.Transport, error) {
	dist, err := loadFixtureDist()
	if err != nil {
		return nil, err
	}
	store := &fixtureStore{files: map[string][]byte{}}
	for url, encoded := range dist.Files {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", url, err)
		}
		store.files[url] = raw
	}
	if fault := os.Getenv(fixtureFaultName); fault != "" {
		if err := store.applyFault(fault); err != nil {
			return nil, err
		}
	}
	return store, nil
}

type fixtureDist struct {
	Key         string            `json:"key"`
	Fingerprint string            `json:"fingerprint"`
	Files       map[string]string `json:"files"`
}

func loadFixtureDist() (fixtureDist, error) {
	directory := os.Getenv(fixtureDistName)
	if directory == "" {
		return fixtureDist{}, fmt.Errorf("the fixture build needs %s", fixtureDistName)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "dist.json"))
	if err != nil {
		return fixtureDist{}, err
	}
	var dist fixtureDist
	if err := json.Unmarshal(raw, &dist); err != nil {
		return fixtureDist{}, err
	}
	if dist.Fingerprint == "" || len(dist.Files) == 0 {
		return fixtureDist{}, errors.New("the fixture distribution is empty")
	}
	return dist, nil
}

// fixtureStore serves exactly the URLs the distribution names. An unlisted URL is
// a refusal, never a fallback: inventing bytes for an endpoint the fixture does
// not carry is how a live downloader would slip through a green suite.
type fixtureStore struct {
	files    map[string][]byte
	failures map[string]string
	slow     bool
}

func (s *fixtureStore) Fetch(ctx context.Context, url, destination string, maxBytes int64) error {
	if s.slow && strings.HasSuffix(url, ".toml") {
		// The suite sends SIGTERM while this blocks.
		time.Sleep(60 * time.Second)
	}
	if message, failed := s.failures[url]; failed {
		return fmt.Errorf("fetch %s: %s", url, message)
	}
	raw, ok := s.files[url]
	if !ok {
		return fmt.Errorf("fetch %s: refused endpoint", url)
	}
	if int64(len(raw)) > maxBytes {
		return fmt.Errorf("fetch %s: response exceeds %d bytes", url, maxBytes)
	}
	return os.WriteFile(destination, raw, 0o600)
}

// applyFault installs one of the suite's faults. Each is named and applied to the
// store, which is the only thing a fault may change.
func (s *fixtureStore) applyFault(fault string) error {
	switch fault {
	case "bootstrap-arm64":
		s.failures = map[string]string{s.bootstrapURL(toolchain.ARM64): "fixture endpoint refused"}
	case "bootstrap-elf":
		url := s.bootstrapURL(toolchain.ARM64)
		if _, ok := s.files[url]; !ok {
			return fmt.Errorf("fixture fault bootstrap-elf: no %s", url)
		}
		changed := []byte("not an elf at all")
		s.files[url] = changed
		digest := sha256.Sum256(changed)
		s.files[url+".sha256"] = []byte(hex.EncodeToString(digest[:]) + " *./rustup-init\n")
	case "manifest-checksum":
		for url := range s.files {
			if !strings.HasSuffix(url, ".toml.sha256") {
				continue
			}
			name := url[strings.LastIndex(url, "/")+1 : len(url)-len(".sha256")]
			s.files[url] = []byte(strings.Repeat("0", 64) + "  " + name + "\n")
		}
	case "manifest-signature":
		for url := range s.files {
			if strings.HasSuffix(url, ".toml.asc") {
				s.files[url] = []byte("not a signature")
			}
		}
	case "archive-digest":
		for _, url := range s.matching(".tar.xz") {
			raw := s.files[url]
			s.files[url] = append(append([]byte{}, raw[:len(raw)-1]...), 0)
			break
		}
	case "last-signature":
		signatures := s.matching(".tar.xz.asc")
		if len(signatures) == 0 {
			return errors.New("fixture fault last-signature: no archive signature")
		}
		s.files[signatures[len(signatures)-1]] = []byte("not a signature")
	case "bootstrap-bytes":
		url := s.bootstrapURL(toolchain.ARM64)
		raw, ok := s.files[url]
		if !ok {
			return fmt.Errorf("fixture fault bootstrap-bytes: no %s", url)
		}
		changed := append(append([]byte{}, raw...), 0)
		s.files[url] = changed
		digest := sha256.Sum256(changed)
		s.files[url+".sha256"] = []byte(hex.EncodeToString(digest[:]) + " *./rustup-init\n")
	case "slow-manifest":
		s.slow = true
	default:
		return fmt.Errorf("unknown fixture fault %q", fault)
	}
	return nil
}

// matching is every served URL with the suffix, in sorted order so "the first"
// and "the last" are the same URLs the Python fixture named.
func (s *fixtureStore) matching(suffix string) []string {
	urls := make([]string, 0, len(s.files))
	for url := range s.files {
		if strings.HasSuffix(url, suffix) {
			urls = append(urls, url)
		}
	}
	sort.Strings(urls)
	return urls
}

func (s *fixtureStore) bootstrapURL(arch toolchain.Architecture) string {
	return toolchain.ExpectedBootstrapURL(
		toolchain.Semver(toolchain.SupportedRustupVersions[0]), arch)
}
