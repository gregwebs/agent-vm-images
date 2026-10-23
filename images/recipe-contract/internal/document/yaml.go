package document

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"go.yaml.in/yaml/v3"
)

type yamlStyle uint8

const (
	stylePlain yamlStyle = iota
	styleNonPlain
)

func styleOf(node *yaml.Node) yamlStyle {
	if node.Style == 0 {
		return stylePlain
	}
	return styleNonPlain
}

// parseYAML runs the lexical guard, decodes exactly one document and applies
// the policies yaml.v3 cannot express as decoder options.
func parseYAML(source []byte) (Value, error) {
	_, value, err := parsePoliced(source)
	return value, err
}

func parsePoliced(source []byte) (*yaml.Node, Value, error) {
	if err := scanYAMLSource(source); err != nil {
		return nil, Value{}, err
	}
	root, err := decodeSingleDocument(source)
	if err != nil {
		return nil, Value{}, err
	}
	if err := checkDuplicateKeys(root, 0); err != nil {
		return nil, Value{}, err
	}
	if err := checkNodePolicies(root, 0); err != nil {
		return nil, Value{}, err
	}
	value, err := nodeValue(root)
	return root, value, err
}

func decodeSingleDocument(source []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	var document yaml.Node
	err := decodeNode(decoder, &document)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var extra yaml.Node
	err = decodeNode(decoder, &extra)
	if errors.Is(err, io.EOF) {
		return documentContent(&document), nil
	}
	if err != nil {
		return nil, err
	}
	//nolint:staticcheck // the retired parser's text; callers and tests match it.
	return nil, errors.New("Source contains multiple documents")
}

// decodeNode turns a decoder panic into a diagnostic. yaml.v3 is trusted to
// report malformed input as an error, but a panic would otherwise escape the
// top-level handler as a status-2 crash instead of the status-1 contract.
func decodeNode(decoder *yaml.Decoder, node *yaml.Node) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.New("yaml: internal parser failure")
		}
	}()
	return decoder.Decode(node)
}

func documentContent(document *yaml.Node) *yaml.Node {
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return nil
	}
	return document.Content[0]
}

type scalarKey struct {
	kind    Kind
	boolean bool
	number  float64
	text    JSString
}

// checkDuplicateKeys compares resolved scalar keys with JavaScript `===`:
// `0x1`/`1` collide, `1`/`"1"` do not, and NaN never equals itself.
func checkDuplicateKeys(node *yaml.Node, depth int) error {
	if depth > maxDepth {
		return errors.New("input exceeds depth100")
	}
	if node == nil {
		return nil
	}
	switch node.Kind { //nolint:exhaustive // yaml.Kind is upstream; the remaining kinds need no work here.
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := checkDuplicateKeys(child, depth); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		seen := make(map[scalarKey]bool, len(node.Content)/2)
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			resolved := nodeValueOfScalar(key)
			if resolved.Kind() != Undefined && !(resolved.Kind() == Number && math.IsNaN(resolved.number)) {
				identity := scalarKey{kind: resolved.kind, boolean: resolved.boolean, number: resolved.number, text: resolved.text}
				if seen[identity] {
					return fmt.Errorf("Map keys must be unique at line %d, column %d", key.Line, key.Column) //nolint:staticcheck // retired diagnostic
				}
				seen[identity] = true
			}
			if err := checkDuplicateKeys(node.Content[index+1], depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := checkDuplicateKeys(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkNodePolicies is the retired walk(): depth, anchors/aliases/tags, string
// keys and the merge key, in the same order so the same diagnostic wins.
func checkNodePolicies(node *yaml.Node, depth int) error {
	if depth > maxDepth {
		return errors.New("input exceeds depth100")
	}
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" || node.Style&yaml.TaggedStyle != 0 {
		return errTagForbidden
	}
	switch node.Kind { //nolint:exhaustive // yaml.Kind is upstream; the remaining kinds need no work here.
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := checkNodePolicies(child, depth); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			resolved := nodeValueOfScalar(key)
			if resolved.Kind() != String {
				return errors.New("mapping keys must be strings")
			}
			if text, _ := resolved.Text(); text.Equal(mergeKey) {
				return errors.New("merge key forbidden")
			}
			// The mapping starts at its syntax indentation; an explicit or
			// multiline key's scalar column is not the enclosing indentation.
			if err := checkBlockIndent(value, node.Column); err != nil {
				return err
			}
			if err := checkNodePolicies(key, depth+1); err != nil {
				return err
			}
			if err := checkNodePolicies(value, depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := checkBlockIndent(child, node.Column); err != nil {
				return err
			}
			if err := checkNodePolicies(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkBlockIndent(node *yaml.Node, parentColumn int) error {
	if node.Kind == yaml.ScalarNode && node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 && node.Column <= parentColumn {
		return errors.New("block-scalar header must be indented past its parent")
	}
	return nil
}

var mergeKey = MustStringFromUTF8("<<")

func nodeValueOfScalar(node *yaml.Node) Value {
	if node == nil || node.Kind != yaml.ScalarNode {
		return UndefinedValue()
	}
	return resolveScalar(styleOf(node), node.Value)
}

// nodeValue converts a policy-checked node tree. Keys take the property-key
// conversion the retired toJS applied, so a numeric key stays accepted as the
// string JavaScript objects require.
func nodeValue(node *yaml.Node) (Value, error) {
	if node == nil {
		return NullValue(), nil
	}
	switch node.Kind { //nolint:exhaustive // yaml.Kind is upstream; the remaining kinds need no work here.
	case yaml.DocumentNode:
		return nodeValue(documentContent(node))
	case yaml.ScalarNode:
		return resolveScalar(styleOf(node), node.Value), nil
	case yaml.SequenceNode:
		items := make([]Value, 0, len(node.Content))
		for _, child := range node.Content {
			item, err := nodeValue(child)
			if err != nil {
				return Value{}, err
			}
			items = append(items, item)
		}
		return NewSequence(items), nil
	case yaml.MappingNode:
		entries := make([]NewMappingEntry, 0, len(node.Content)/2)
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, ok := nodeValueOfScalar(node.Content[index]).Text()
			if !ok {
				return Value{}, errors.New("mapping keys must be strings")
			}
			value, err := nodeValue(node.Content[index+1])
			if err != nil {
				return Value{}, err
			}
			entries = append(entries, NewMappingEntry{Key: key, Value: value})
		}
		return NewMapping(entries...), nil
	default:
		return Value{}, errors.New("mapping keys must be strings")
	}
}
