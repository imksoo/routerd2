package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

// Parse accepts one YAML or JSON configuration, with duplicate/unknown keys
// rejected. Anchors, aliases and merge keys are intentionally unsupported so
// the document an operator sees is the document the kernel receives.
func Parse(b []byte) (Document, error) {
	var d Document
	if err := Decode(bytes.NewReader(b), &d); err != nil {
		return d, err
	}
	return d, Validate(d)
}

func decodeYAML(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("expected exactly one YAML or JSON document")
	}
	count := 0
	value, err := plain(&root, 0, &count)
	if err != nil {
		return err
	}
	j, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return decodeStrictJSON(j, v, maxDocumentNodes)
}

func plain(n *yaml.Node, depth int, count *int) (any, error) {
	*count++
	if depth > 64 || *count > maxDocumentNodes {
		return nil, errors.New("configuration is too deeply nested or complex")
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("line %d: YAML anchors and aliases are not supported", n.Line)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, errors.New("empty configuration")
		}
		return plain(n.Content[0], depth+1, count)
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" {
				return nil, fmt.Errorf("line %d: mapping keys must be plain strings; merge keys are unsupported", key.Line)
			}
			if _, ok := out[key.Value]; ok {
				return nil, fmt.Errorf("line %d: duplicate key %q", key.Line, key.Value)
			}
			value, err := plain(n.Content[i+1], depth+1, count)
			if err != nil {
				return nil, err
			}
			out[key.Value] = value
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			value, err := plain(c, depth+1, count)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str", "!!bool", "!!int", "!!float", "!!null":
			var value any
			if err := n.Decode(&value); err != nil {
				return nil, err
			}
			return value, nil
		case "!!timestamp":
			return n.Value, nil // persisted RFC3339 timestamps remain strings for JSON decoding
		default:
			return nil, fmt.Errorf("line %d: YAML tag %q is not supported", n.Line, n.Tag)
		}
	default:
		return nil, errors.New("unsupported YAML node")
	}
}

// YAML emits the canonical running configuration. It preserves semantic values,
// not source formatting or comments; session editors must make this explicit.
func YAML(d Document) ([]byte, error) {
	j, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err = yaml.Unmarshal(j, &n); err != nil {
		return nil, err
	}
	var block func(*yaml.Node)
	block = func(n *yaml.Node) {
		n.Style = 0
		for _, c := range n.Content {
			block(c)
		}
	}
	block(&n)
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err = e.Encode(&n); err != nil {
		return nil, err
	}
	if err = e.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
