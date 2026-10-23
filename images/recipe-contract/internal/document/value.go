package document

import (
	"fmt"
	"math"
	"sort"
)

// Kind is the JavaScript value kind the parsers produce. Undefined and Null are
// distinct because the policies use `!== undefined` for "present but null must
// still fail" (the retired policy reader's manifest checks).
type Kind uint8

const (
	Undefined Kind = iota
	Null
	Bool
	Number
	String
	Sequence
	Mapping
)

func (k Kind) String() string {
	switch k {
	case Undefined:
		return "undefined"
	case Null:
		return "null"
	case Bool:
		return "boolean"
	case Number:
		return "number"
	case String:
		return "string"
	case Sequence:
		return "sequence"
	case Mapping:
		return "mapping"
	default:
		return fmt.Sprintf("Kind(%d)", k)
	}
}

// Value is a parsed JSON or YAML value with JavaScript semantics.
type Value struct {
	kind    Kind
	boolean bool
	number  float64
	text    JSString
	items   []Value
	mapping *mapping
}

type mapping struct {
	order  []JSString
	values map[JSString]Value
}

var undefined = Value{kind: Undefined}

// UndefinedValue is the absent value; it is only equal to itself.
func UndefinedValue() Value { return undefined }

// NullValue is the parsed `null`.
func NullValue() Value { return Value{kind: Null} }

// NewBool is a parsed boolean.
func NewBool(value bool) Value { return Value{kind: Bool, boolean: value} }

// NewNumber is a parsed number.
func NewNumber(value float64) Value { return Value{kind: Number, number: value} }

// NewString wraps already-validated UTF-8 text.
func NewString(text string) Value {
	value, err := StringFromUTF8(text)
	if err != nil {
		panic(err)
	}
	return NewJSString(value)
}

// NewJSString wraps raw code units.
func NewJSString(text JSString) Value { return Value{kind: String, text: text} }

// NewSequence builds a sequence value.
func NewSequence(items []Value) Value {
	copied := make([]Value, len(items))
	copy(copied, items)
	return Value{kind: Sequence, items: copied}
}

// NewMappingEntry is one source-ordered mapping entry.
type NewMappingEntry struct {
	Key   JSString
	Value Value
}

// NewMapping builds a mapping from entries in source order.
func NewMapping(entries ...NewMappingEntry) Value {
	values := make(map[JSString]Value, len(entries))
	order := make([]JSString, 0, len(entries))
	for _, entry := range entries {
		if _, exists := values[entry.Key]; !exists {
			order = append(order, entry.Key)
		}
		values[entry.Key] = entry.Value
	}
	return Value{kind: Mapping, mapping: &mapping{order: order, values: values}}
}

// Kind reports the value kind.
func (v Value) Kind() Kind { return v.kind }

// Text returns the string payload.
func (v Value) Text() (JSString, bool) {
	if v.kind != String {
		return JSString{}, false
	}
	return v.text, true
}

// Bool returns the boolean payload.
func (v Value) Bool() (bool, bool) {
	if v.kind != Bool {
		return false, false
	}
	return v.boolean, true
}

// Number returns the numeric payload.
func (v Value) Number() (float64, bool) {
	if v.kind != Number {
		return 0, false
	}
	return v.number, true
}

// Items returns the sequence payload.
func (v Value) Items() []Value {
	if v.kind != Sequence {
		return nil
	}
	return v.items
}

// IsMapping reports whether the value is a mapping.
func (v Value) IsMapping() bool { return v.kind == Mapping }

// Get looks a key up with a UTF-8 key, the convenience path for policy code.
func (v Value) Get(key string) Value {
	encoded, err := StringFromUTF8(key)
	if err != nil {
		return undefined
	}
	return v.GetJS(encoded)
}

// GetJS looks a key up by raw code units; absent keys are Undefined.
func (v Value) GetJS(key JSString) Value {
	if v.kind != Mapping {
		return undefined
	}
	value, ok := v.mapping.values[key]
	if !ok {
		return undefined
	}
	return value
}

// Has is Object.hasOwn.
func (v Value) Has(key string) bool {
	encoded, err := StringFromUTF8(key)
	if err != nil {
		return false
	}
	return v.HasJS(encoded)
}

// HasJS is Object.hasOwn with a raw key.
func (v Value) HasJS(key JSString) bool {
	if v.kind != Mapping {
		return false
	}
	_, ok := v.mapping.values[key]
	return ok
}

// Keys returns the own-property keys in JavaScript order: canonical array-index
// keys ascending, then the remaining keys in insertion order. The retired
// `keys()` helper reported the first unexpected key in this order.
func (v Value) Keys() []JSString {
	if v.kind != Mapping {
		return nil
	}
	indices := make([]int, 0, len(v.mapping.order))
	others := make([]JSString, 0, len(v.mapping.order))
	for _, key := range v.mapping.order {
		if index, ok := arrayIndex(key); ok {
			indices = append(indices, index)
			continue
		}
		others = append(others, key)
	}
	sort.Ints(indices)
	keys := make([]JSString, 0, len(v.mapping.order))
	for _, index := range indices {
		keys = append(keys, keyOfArrayIndex(index))
	}
	return append(keys, others...)
}

// stringKeys returns the keys in JavaScript order as UTF-8 text, failing when a
// key is not valid UTF-8 (a raw key can be an unpaired surrogate).
func (v Value) stringKeys() ([]string, error) {
	keys := v.Keys()
	text := make([]string, len(keys))
	for index, key := range keys {
		value, ok := key.UTF8()
		if !ok {
			return nil, fmt.Errorf("key is not valid UTF-8")
		}
		text[index] = value
	}
	return text, nil
}

// Without copies the mapping without the named keys (JavaScript `delete`).
func (v Value) Without(keys ...string) Value {
	dropped := make([]JSString, 0, len(keys))
	for _, key := range keys {
		if encoded, err := StringFromUTF8(key); err == nil {
			dropped = append(dropped, encoded)
		}
	}
	return v.WithoutJS(dropped...)
}

// WithoutJS is Without for raw keys.
func (v Value) WithoutJS(keys ...JSString) Value {
	if v.kind != Mapping {
		return v
	}
	dropped := make(map[JSString]bool, len(keys))
	for _, key := range keys {
		dropped[key] = true
	}
	order := make([]JSString, 0, len(v.mapping.order))
	values := make(map[JSString]Value, len(v.mapping.values))
	for _, key := range v.mapping.order {
		if dropped[key] {
			continue
		}
		order = append(order, key)
		values[key] = v.mapping.values[key]
	}
	return Value{kind: Mapping, mapping: &mapping{order: order, values: values}}
}

// With copies the mapping with one key set, replacing in place (JavaScript
// assignment) or appending.
func (v Value) With(key string, replacement Value) Value {
	encoded, err := StringFromUTF8(key)
	if err != nil {
		panic(err)
	}
	return v.WithJS(encoded, replacement)
}

// WithJS is With for a raw key.
func (v Value) WithJS(key JSString, replacement Value) Value {
	order := make([]JSString, 0, len(v.mapping.order)+1)
	values := make(map[JSString]Value, len(v.mapping.values)+1)
	found := false
	for _, existing := range v.mapping.order {
		order = append(order, existing)
		if existing.Equal(key) {
			values[existing] = replacement
			found = true
			continue
		}
		values[existing] = v.mapping.values[existing]
	}
	if !found {
		order = append(order, key)
		values[key] = replacement
	}
	return Value{kind: Mapping, mapping: &mapping{order: order, values: values}}
}

// Clone deep-copies so edits cannot alias the parsed document.
func (v Value) Clone() Value {
	switch v.kind {
	case Undefined, Null, Bool, Number, String:
		return v
	case Sequence:
		items := make([]Value, len(v.items))
		for index, item := range v.items {
			items[index] = item.Clone()
		}
		return Value{kind: Sequence, items: items}
	case Mapping:
		entries := make([]NewMappingEntry, 0, len(v.mapping.order))
		for _, key := range v.mapping.order {
			entries = append(entries, NewMappingEntry{Key: key, Value: v.mapping.values[key].Clone()})
		}
		return NewMapping(entries...)
	default:
		panic("document: unhandled value kind")
	}
}

// Describe is the assertion "got" text: JSON, or `undefined`.
func (v Value) Describe() string {
	if v.kind == Undefined {
		return "undefined"
	}
	return string(v.AppendJS(nil))
}

// arrayIndex reports whether key is a JavaScript canonical array index.
func arrayIndex(key JSString) (int, bool) {
	text, ok := key.ASCII()
	if !ok || text == "" || len(text) > 10 {
		return 0, false
	}
	if text == "0" {
		return 0, true
	}
	if text[0] < '1' || text[0] > '9' {
		return 0, false
	}
	value := 0
	for index := 0; index < len(text); index++ {
		digit := text[index]
		if digit < '0' || digit > '9' {
			return 0, false
		}
		value = value*10 + int(digit-'0')
	}
	if value > math.MaxUint32-1 {
		return 0, false
	}
	return value, true
}

func keyOfArrayIndex(index int) JSString {
	return MustStringFromUTF8(fmt.Sprintf("%d", index))
}

// JSKeyOrder sorts keys by JavaScript property order.
func JSKeyOrder(keys []JSString) []JSString {
	value := NewMapping()
	for _, key := range keys {
		value = value.WithJS(key, UndefinedValue())
	}
	return value.Keys()
}
