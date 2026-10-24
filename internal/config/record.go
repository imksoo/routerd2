package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const maxDocumentNodes = 100000

// Persisted records are JSON, independent of the operator-facing YAML parser.
// Stream the shape check before typed decoding to reject duplicate keys and
// unbounded nesting without constructing an intermediate YAML node tree. The
// record budget includes two individually bounded documents and their metadata.
func decodeRecord(data []byte, record *Committed) error {
	if err := checkJSONShape(data, 2*maxDocumentNodes+1024); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(record)
}

func checkJSONShape(data []byte, maximum int) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	count := 0
	var value func(int) error
	value = func(depth int) error {
		count++
		if depth > 64 || count > maximum {
			return errors.New("configuration is too deeply nested or complex")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("configuration contains a duplicate or invalid JSON key")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON closing delimiter")
		}
		_, err = d.Token() // encoding/json checks the matching closing delimiter.
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("expected exactly one JSON document")
	}
	return nil
}
