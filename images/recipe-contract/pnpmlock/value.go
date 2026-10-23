package pnpmlock

import (
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// The policy helpers below are the retired assert helpers. Their texts are part
// of the diagnostic contract (§ 5.8), so they are written once here.

func recordOf(value document.Value) (document.Value, error) {
	if !value.IsMapping() {
		return document.Value{}, errorf("expected mapping")
	}
	return value, nil
}

func textOf(value document.Value) (document.JSString, error) {
	if text, ok := value.Text(); ok {
		return text, nil
	}
	return document.JSString{}, errorf("expected string")
}

func displayOf(text document.JSString) string { return text.Display() }

func isUndefined(value document.Value) bool { return value.Kind() == document.Undefined }

func isTrue(value document.Value) bool {
	boolean, ok := value.Bool()
	return ok && boolean
}

// checkKeys reports the first unexpected key in JavaScript property order, then
// the first missing required key, exactly as the retired keys() helper did.
func checkKeys(value document.Value, allowed, required []string) error {
	permitted := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		permitted[key] = true
	}
	for _, key := range value.Keys() {
		if !permitted[key.Display()] {
			return errorf("unexpected key: %s", key.Display())
		}
	}
	for _, key := range required {
		if !value.Has(key) {
			return errorf("missing key: %s", key)
		}
	}
	return nil
}

func strictEqual(a, b document.Value) bool { return document.StrictEqual(a, b) }

func deepEqual(a, b document.Value) bool { return document.DeepStrictEqual(a, b) }

// sortedKeys returns the mapping's keys sorted with JavaScript `<`.
func sortedKeys(value document.Value) []document.JSString {
	keys := value.Keys()
	sortJSStrings(keys)
	return keys
}

func sortJSStrings(keys []document.JSString) {
	for index := 1; index < len(keys); index++ {
		for cursor := index; cursor > 0 && keys[cursor].Less(keys[cursor-1]); cursor-- {
			keys[cursor], keys[cursor-1] = keys[cursor-1], keys[cursor]
		}
	}
}

// jsStringValue is the value every typed identity uses when it is re-emitted.
func jsStringValue(text string) document.Value {
	value, err := document.StringFromUTF8(text)
	if err != nil {
		panic("pnpmlock: text is not valid UTF-8")
	}
	return document.NewJSString(value)
}

func boolValue(boolean bool) document.Value { return document.NewBool(boolean) }

func sequenceValue(items []document.Value) document.Value { return document.NewSequence(items) }

type mappingEntry = document.NewMappingEntry

func mappingValue(entries ...mappingEntry) document.Value { return document.NewMapping(entries...) }

func entry(key string, value document.Value) mappingEntry {
	encoded, err := document.StringFromUTF8(key)
	if err != nil {
		panic("pnpmlock: key is not valid UTF-8")
	}
	return mappingEntry{Key: encoded, Value: value}
}

// stringOf reports the UTF-8 text of a value when it is a string.
func stringOf(value document.Value) (string, error) {
	text, err := textOf(value)
	if err != nil {
		return "", err
	}
	return text.Display(), nil
}
