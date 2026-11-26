package closure

import (
	"strconv"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/strictjson"
)

// This file is the pin document's shape rules: exact key sets, exact types, and
// Python's `type(value) is int` for the one numeric field. `pin.json` is read by
// both this reader and the Python oracle until #254, so the wording is the
// Python's where a suite matches it.

type members = map[string]strictjson.Value

// requireKeys enforces an exact key set: every required name present, and no name
// outside required plus optional.
func requireKeys(node any, required, optional []string, what string) (members, error) {
	mapping, ok := node.(map[string]strictjson.Value)
	if !ok {
		return nil, refuse("%s: expected an object", what)
	}
	for _, key := range required {
		if _, present := mapping[key]; !present {
			return nil, refuse("%s: missing %s", what, repr(key))
		}
	}
	allowed := make(map[string]struct{}, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = struct{}{}
	}
	for _, key := range optional {
		allowed[key] = struct{}{}
	}
	unknown := []string{}
	for key := range mapping {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		return nil, refuse("%s: unknown key %s", what, repr(sortedFirst(unknown)))
	}
	return mapping, nil
}

// fieldOf reads one member and names the missing key with its own context.
func fieldOf(node any, key, what string) (any, error) {
	mapping, ok := node.(map[string]strictjson.Value)
	if !ok {
		return nil, refuse("%s: expected an object", what)
	}
	value, present := mapping[key]
	if !present {
		return nil, refuse("%s: missing %s", what, repr(key))
	}
	return value, nil
}

// exactString is Python's `exact_type(value, str)`.
func exactString(value any, what string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", refuse("%s: expected str", what)
	}
	return text, nil
}

// exactInt is Python's `type(value) is int`: a fraction, an exponent, a float or a
// bool is not an integer whatever it evaluates to.
func exactInt(value any, what string) (int, error) {
	number, ok := value.(strictjson.Number)
	if !ok || !number.Integer {
		return 0, refuse("%s: expected an integer", what)
	}
	parsed, err := strconv.Atoi(number.Literal)
	if err != nil {
		return 0, refuse("%s: expected an integer", what)
	}
	return parsed, nil
}

// exactList is Python's `type(value) is list`.
func exactList(value any, what string) ([]any, error) {
	items, ok := value.([]strictjson.Value)
	if !ok {
		return nil, refuse("%s: expected list", what)
	}
	return items, nil
}

// stringList is a list of identity strings: each one must be a string with no
// control character.
func stringList(value any, what string) ([]string, error) {
	items, err := exactList(value, what)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, refuse("%s: expected strings", what)
		}
		if err := rejectControlCharacters(text, what); err != nil {
			return nil, err
		}
		texts = append(texts, text)
	}
	return texts, nil
}

// sortedFirst is the first name in sorted order, which is the one Python names.
func sortedFirst(names []string) string {
	first := names[0]
	for _, name := range names[1:] {
		if name < first {
			first = name
		}
	}
	return first
}
