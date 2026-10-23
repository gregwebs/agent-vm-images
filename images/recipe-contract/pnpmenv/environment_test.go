package pnpmenv

import (
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckEnvironment(t *testing.T) {
	for _, vector := range []struct {
		name    string
		environ []string
		want    string
	}{
		{"clean", []string{"PATH=/usr/bin", "HOME=/root"}, ""},
		{"launcher-default", []string{"PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440"}, ""},
		{"launcher-default-empty", []string{"PNPM_CONFIG_MINIMUM_RELEASE_AGE="}, ""},
		{"launcher-default-wrong", []string{"PNPM_CONFIG_MINIMUM_RELEASE_AGE=1"}, "unsupported package-manager environment: PNPM_CONFIG_MINIMUM_RELEASE_AGE"},
		{"case-sensitive-default", []string{"pnpm_config_minimum_release_age=1440"}, "unsupported package-manager environment: pnpm_config_minimum_release_age"},
		{"npm-config", []string{"npm_config_registry=https://evil.invalid"}, "unsupported package-manager environment: npm_config_registry"},
		{"npm-config-upper", []string{"NPM_CONFIG_REGISTRY=x"}, "unsupported package-manager environment: NPM_CONFIG_REGISTRY"},
		{"corepack", []string{"COREPACK_HOME=/tmp"}, "unsupported package-manager environment: COREPACK_HOME"},
		{"pnpm-home", []string{"PNPM_HOME=/tmp"}, "unsupported package-manager environment: PNPM_HOME"},
		{"kelvin", []string{"pnpm\u212ahome=/tmp"}, ""},
		{"node-options", []string{"NODE_OPTIONS=--require=/tmp/x"}, "unsupported manager runtime override: NODE_OPTIONS"},
		{"node-options-empty", []string{"NODE_OPTIONS="}, ""},
		{"node-path", []string{"NODE_PATH=/tmp"}, "unsupported manager runtime override: NODE_PATH"},
		{"bun-options", []string{"BUN_OPTIONS=--x"}, "unsupported manager runtime override: BUN_OPTIONS"},
		{"tls", []string{"NODE_TLS_REJECT_UNAUTHORIZED=0"}, "TLS verification cannot be disabled"},
		{"tls-one", []string{"NODE_TLS_REJECT_UNAUTHORIZED=1"}, ""},
		{"no-equals", []string{"BROKEN"}, ""},
	} {
		err := Check(vector.environ, nil)
		switch {
		case vector.want == "" && err != nil:
			t.Errorf("%s = %v", vector.name, err)
		case vector.want != "" && (err == nil || !strings.Contains(err.Error(), vector.want)):
			t.Errorf("%s = %v, want %q", vector.name, err, vector.want)
		}
	}
}

func TestCheckPrefixes(t *testing.T) {
	directory := t.TempDir()
	if err := Check(nil, []pnpmlock.Prefix{pnpmlock.Prefix(directory)}); err != nil {
		t.Fatalf("clean prefix = %v", err)
	}
	for _, file := range unreviewedFiles {
		prefix := t.TempDir()
		if err := os.WriteFile(filepath.Join(prefix, file), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Check(nil, []pnpmlock.Prefix{pnpmlock.Prefix(directory), pnpmlock.Prefix(prefix)}); err == nil || !strings.Contains(err.Error(), "unreviewed package-manager input: "+file) {
			t.Errorf("%s = %v", file, err)
		}
	}
	// An absent prefix is not a failure (lstat reports ENOENT), but an lstat
	// error other than ENOENT is: a prefix that is a file cannot contain the
	// probe path, so ENOTDIR surfaces as the Node-shaped diagnostic.
	if err := Check(nil, []pnpmlock.Prefix{pnpmlock.Prefix(filepath.Join(directory, "absent"))}); err != nil {
		t.Errorf("absent prefix = %v", err)
	}
	file := filepath.Join(directory, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(nil, []pnpmlock.Prefix{pnpmlock.Prefix(file)}); err == nil || !strings.Contains(err.Error(), "ENOTDIR") {
		t.Errorf("file prefix = %v", err)
	}
}
