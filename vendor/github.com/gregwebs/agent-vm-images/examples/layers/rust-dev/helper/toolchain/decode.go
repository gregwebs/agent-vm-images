package toolchain

// Strict decoding and typed access. Every helper is a port of one function in
// the source reader, including its exact-type rule: a TOML date is not a string
// and `true` is not an integer, whatever a converter would do.

import (
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/strictjson"
)

// decodeJSON reads pin.json with duplicate keys, a BOM, invalid UTF-8 and
// trailing input refused.
func decodeJSON(raw []byte, what string) (any, error) {
	value, err := strictjson.Decode(raw, what)
	return value, adopt(err)
}

// decodeTOML reads a TOML 1.0 document: a leading BOM and invalid UTF-8 are
// refused before the decode, as the source reader refuses them.
//
// The decoder is vendored go-toml/v2 v2.2.4, measured against `tomllib` on the
// nine grammar vectors in docs/adr/0017-go-programs.md. It matches TOML 1.0 with
// strict table semantics on every row but one: it accepts the TOML 1.1 `\e`
// escape, which no go-toml/v2 release refuses. That acceptance is a declared
// divergence with a test that pins the value it produces
// (TestTOMLGrammar/escape-e).
func decodeTOML(raw []byte, what string) (map[string]any, error) {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return nil, refuse("%s: leading UTF-8 BOM is not accepted", what)
	}
	if !utf8.Valid(raw) {
		return nil, refuse("%s: not valid UTF-8", what)
	}
	document := map[string]any{}
	if err := toml.Unmarshal(raw, &document); err != nil {
		return nil, refuse("%s: not valid TOML: %v", what, err)
	}
	return document, nil
}

// fieldOf reads one member, naming the field path in both refusals.
func fieldOf(node any, key, what string) (any, error) {
	table, ok := node.(map[string]any)
	if !ok {
		return nil, refuse("%s: expected an object", what)
	}
	value, present := table[key]
	if !present {
		return nil, refuse("%s: missing %s", what, strictjson.Repr(key))
	}
	return value, nil
}

// exactText requires Python's `type(value) is str`.
func exactTextField(node any, what string) (string, error) {
	text, ok := node.(string)
	if !ok {
		return "", refuse("%s: expected str", what)
	}
	return text, nil
}

// exactInt requires Python's `type(value) is int`: a JSON whole-number literal,
// or a TOML integer. A fraction, an exponent, a boolean or a string is not one.
//
// The value is a big.Int, not an int64, because Python's int has no range: a pin
// with `"schema": 18446744073709551616` is an integer that is not 1, which is
// `unsupported schema`, not `expected an integer`.
func exactInt(node any, what string) (*big.Int, error) {
	switch value := node.(type) {
	case int64:
		return big.NewInt(value), nil
	case strictjson.Number:
		if !value.Integer {
			return nil, refuse("%s: expected an integer", what)
		}
		if integer, ok := new(big.Int).SetString(value.Literal, 10); ok {
			return integer, nil
		}
	}
	return nil, refuse("%s: expected an integer", what)
}

// stringList requires a list of strings, refusing a control character in each.
func stringList(node any, what string) ([]string, error) {
	items, ok := node.([]any)
	if !ok {
		return nil, refuse("%s: expected a list", what)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, refuse("%s: expected strings", what)
		}
		if err := RejectControlCharacters(text, what); err != nil {
			return nil, err
		}
		result = append(result, text)
	}
	return result, nil
}

// requireKeys is the exact-key rule at one object boundary: unknown keys are
// refused sorted, then missing keys in the caller's order.
func requireKeys(node any, required, optional []string, what string) (map[string]any, error) {
	table, ok := node.(map[string]any)
	if !ok {
		return nil, refuse("%s: expected an object", what)
	}
	var unknown []string
	for key := range table {
		if !contains(required, key) && !contains(optional, key) {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	if len(unknown) != 0 {
		return nil, refuse("%s: unknown keys: %s", what, strings.Join(unknown, ", "))
	}
	var missing []string
	for _, key := range required {
		if _, present := table[key]; !present {
			missing = append(missing, key)
		}
	}
	if len(missing) != 0 {
		return nil, refuse("%s: missing %s", what, strings.Join(missing, ", "))
	}
	return table, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// lookup walks a `.get()` chain, where a missing key is absent and a non-table
// on the way is a refusal (the source reader raises AttributeError there: a
// reported P1 divergence, status 1 either way).
func lookup(node any, keys ...string) (any, bool, error) {
	current := node
	for _, key := range keys {
		table, ok := current.(map[string]any)
		if !ok {
			return nil, false, refuse("expected an object at %s", key)
		}
		value, present := table[key]
		if !present {
			return nil, false, nil
		}
		current = value
	}
	return current, true, nil
}
