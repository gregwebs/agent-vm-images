// Package document parses the JSON and YAML documents the pnpm policies read.
// It keeps yaml.v3 behind one seam and reproduces the retired npm yaml policies
// (duplicate keys, merge:false, tag/anchor rejection, depth and byte bounds) and
// JavaScript value semantics, including UTF-16 strings with unpaired surrogates.
package document

import (
	"bytes"
	"errors"
	"unicode/utf8"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
)

// Format selects the grammar. JSON inputs are read with the bounded RFC 8259
// reader because yaml.v3 rejects valid JSON (`\/`, surrogate escapes, long flow
// keys) that the retired parser accepted.
type Format uint8

const (
	YAML Format = iota
	JSON
)

// Parse parses bounded, UTF-8 source.
func Parse(source []byte, format Format) (Value, error) {
	prepared, err := prepare(source)
	if err != nil {
		return Value{}, err
	}
	switch format {
	case YAML:
		return parseYAML(prepared)
	case JSON:
		return parseJSON(prepared)
	default:
		panic("document: unhandled format")
	}
}

// ReadFile reads a bounded regular file and parses it.
func ReadFile(path string, format Format) (Value, error) {
	source, err := nodefs.ReadBounded(path)
	if err != nil {
		return Value{}, err
	}
	return Parse(source, format)
}

// prepare validates UTF-8 fatally and strips one byte-order mark, which is what
// TextDecoder{ignoreBOM: false} did before either parser saw the source.
func prepare(source []byte) ([]byte, error) {
	if !utf8.Valid(source) {
		//nolint:staticcheck // the retired TextDecoder text; policy diagnostics match it.
		return nil, errors.New("The encoded data was not valid for encoding utf-8")
	}
	return bytes.TrimPrefix(source, []byte("\xef\xbb\xbf")), nil
}
