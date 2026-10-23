// Package pnpmenv rejects caller package-manager policy and unreviewed
// package-manager inputs before any package-manager process starts.
package pnpmenv

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

// defaults are the launcher's own settings. A caller may pass the same value
// through, never a different one, and the lookup is case-sensitive because these
// are the exact names the launcher sets.
var defaults = map[string]string{
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE":                     "1440",
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT":              "true",
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME": "false",
	"PNPM_CONFIG_TRUST_POLICY":                            "no-downgrade",
	"PNPM_CONFIG_IGNORE_SCRIPTS":                          "true",
}

// unreviewedFiles are package-manager inputs that would execute or redirect the
// install; a reviewed project carries none of them.
var unreviewedFiles = []string{".npmrc", ".pnpmfile.cjs", ".pnpmfile.js", ".pnpmfile.mjs", "pnpmfile.cjs"}

// runtimeOverrides are environment variables that would change how Node or Bun
// loads code.
var runtimeOverrides = []string{"NODE_OPTIONS", "NODE_PATH", "BUN_OPTIONS"}

// Check validates the whole environment, then every prefix argument.
func Check(environ []string, prefixes []pnpmlock.Prefix) error {
	type variable struct{ name, value string }
	variables := make([]variable, 0, len(environ))
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			variables = append(variables, variable{name: name, value: value})
		}
	}
	for _, entry := range variables {
		key, value := entry.name, entry.value
		lowered := asciiLower(key)
		managed := strings.HasPrefix(lowered, "npm_config_") ||
			strings.HasPrefix(lowered, "pnpm_config_") ||
			strings.HasPrefix(lowered, "corepack_") ||
			lowered == "pnpm_home"
		if !managed {
			continue
		}
		expected, known := defaults[key]
		if !known || (value != "" && value != expected) {
			return fmt.Errorf("unsupported package-manager environment: %s", key)
		}
	}
	for _, key := range runtimeOverrides {
		for _, entry := range variables {
			if entry.name == key && entry.value != "" {
				return fmt.Errorf("unsupported manager runtime override: %s", key)
			}
		}
	}
	for _, entry := range variables {
		if entry.name == "NODE_TLS_REJECT_UNAUTHORIZED" && entry.value == "0" {
			return fmt.Errorf("TLS verification cannot be disabled")
		}
	}
	for _, prefix := range prefixes {
		for _, file := range unreviewedFiles {
			present, err := nodefs.Present(filepath.Join(string(prefix), file))
			if err != nil {
				return err
			}
			if present {
				return fmt.Errorf("unreviewed package-manager input: %s", file)
			}
		}
	}
	return nil
}

// asciiLower folds only ASCII, because Go's `(?i)` also folds U+212A KELVIN to
// `k`, which would widen the managed-variable set.
func asciiLower(text string) string {
	lowered := make([]byte, len(text))
	for index := 0; index < len(text); index++ {
		c := text[index]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lowered[index] = c
	}
	return string(lowered)
}
