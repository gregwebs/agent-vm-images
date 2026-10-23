package document

import (
	"bytes"
	"errors"

	"go.yaml.in/yaml/v3"
)

// ReplaceTopLevel rewrites one top-level mapping value and leaves every other
// byte to the encoder. The result is re-parsed and compared with the expected
// value: an edit that changed anything else fails closed instead of publishing a
// workspace the reviewer did not see.
func ReplaceTopLevel(source []byte, key string, replacement Value) ([]byte, error) {
	return editTopLevel(source, MustStringFromUTF8(key), &replacement)
}

// DeleteTopLevel removes one top-level mapping value.
func DeleteTopLevel(source []byte, key string) ([]byte, error) {
	return editTopLevel(source, MustStringFromUTF8(key), nil)
}

func editTopLevel(source []byte, key JSString, replacement *Value) ([]byte, error) {
	prepared, err := prepare(source)
	if err != nil {
		return nil, err
	}
	document, original, err := parsePoliced(prepared)
	if err != nil {
		return nil, err
	}
	if document == nil || document.Kind != yaml.MappingNode {
		return nil, errors.New("expected mapping")
	}
	found := false
	for index := 0; index+1 < len(document.Content); index += 2 {
		if text, ok := nodeValueOfScalar(document.Content[index]).Text(); ok && text.Equal(key) {
			found = true
			if replacement == nil {
				document.Content = append(document.Content[:index], document.Content[index+2:]...)
			} else {
				document.Content[index+1] = nodeOfValue(*replacement)
			}
			break
		}
	}
	if !found && replacement != nil {
		document.Content = append(document.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: mustUTF8(key)},
			nodeOfValue(*replacement))
	}
	var expected Value
	if replacement == nil {
		expected = original.WithoutJS(key)
	} else {
		expected = original.WithJS(key, *replacement)
	}
	encoded, err := encodeDocument(document)
	if err != nil {
		return nil, err
	}
	reparsed, err := Parse(encoded, YAML)
	if err != nil {
		return nil, err
	}
	if !DeepStrictEqual(reparsed, expected) {
		return nil, errors.New("workspace edit changed unrelated values")
	}
	return encoded, nil
}

func mustUTF8(value JSString) string {
	text, ok := value.UTF8()
	if !ok {
		panic("document: text is not valid UTF-8")
	}
	return text
}

func encodeDocument(document *yaml.Node) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// nodeOfValue builds presentation nodes for an edited value. Strings carry the
// !!str tag so the encoder quotes anything that would otherwise re-resolve as a
// different kind.
func nodeOfValue(value Value) *yaml.Node {
	switch value.Kind() {
	case Null:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: "null"}
	case Bool:
		if value.boolean {
			return &yaml.Node{Kind: yaml.ScalarNode, Value: "true"}
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Value: "false"}
	case Number:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: mustUTF8(jsNumberString(value.number))}
	case String:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: mustUTF8(value.text)}
	case Sequence:
		node := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range value.items {
			node.Content = append(node.Content, nodeOfValue(item))
		}
		return node
	case Undefined:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: "null"}
	case Mapping:
		node := &yaml.Node{Kind: yaml.MappingNode}
		for _, key := range value.Keys() {
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: mustUTF8(key)},
				nodeOfValue(value.mapping.values[key]))
		}
		return node
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: "null"}
	}
}
