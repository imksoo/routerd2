// Package strictjson validates bounded, unambiguous persisted JSON shapes.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"unicode"
)

// encoding/json accepts case-insensitive aliases for struct fields even with
// DisallowUnknownFields. A typed record must use its actual JSON field names:
// two spellings must not overwrite each other after YAML map normalization.
// Map keys remain operator data, and custom JSON types retain their own schema.
// Decode checks exact typed field names, duplicate keys and finite structure
// before typed decoding. Custom UnmarshalJSON values keep their own schemas.
func Decode(data []byte, target any, maximum int) error {
	if err := checkTypedJSONShape(data, maximum, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// Check validates a generic JSON shape without assigning typed field names.
func Check(data []byte, maximum int) error { return checkTypedJSONShape(data, maximum, nil) }

func checkTypedJSONShape(data []byte, maximum int, target reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	count := 0
	var value func(int, reflect.Type) error
	value = func(depth int, target reflect.Type) error {
		count++
		if depth > 64 || count > maximum {
			return errors.New("JSON record is too deeply nested or complex")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil // The final typed decoder checks scalar kinds and values.
		}
		target = jsonShapeType(target)
		switch delimiter {
		case '{':
			var fields map[string]reflect.Type
			var element reflect.Type
			if target != nil {
				if target.Kind() == reflect.Struct {
					fields = exactJSONFields(target)
				} else if target.Kind() == reflect.Map {
					element = target.Elem()
				}
			}
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("JSON record contains a duplicate or invalid JSON key")
				}
				seen[name] = true
				child := element
				if fields != nil {
					child, ok = fields[name]
					if !ok {
						// Diagnostics must not echo a nearly 2 MiB input key.
						if len(name) > 128 {
							name = name[:128] + "..."
						}
						return fmt.Errorf("json: unknown field %q", name)
					}
				}
				if err := value(depth+1, child); err != nil {
					return err
				}
			}
		case '[':
			var element reflect.Type
			if target != nil && (target.Kind() == reflect.Slice || target.Kind() == reflect.Array) {
				element = target.Elem()
			}
			for decoder.More() {
				if err := value(depth+1, element); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON closing delimiter")
		}
		_, err = decoder.Token() // encoding/json verifies the matching delimiter.
		return err
	}
	if err := value(0, target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("expected exactly one JSON document")
	}
	return nil
}

var jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()

func jsonShapeType(target reflect.Type) reflect.Type {
	for target != nil {
		if target.Implements(jsonUnmarshaler) || reflect.PointerTo(target).Implements(jsonUnmarshaler) {
			return nil
		}
		if target.Kind() != reflect.Pointer {
			return target
		}
		target = target.Elem()
	}
	return nil
}

var exactFieldCache sync.Map // reflect.Type -> immutable map[string]reflect.Type

type jsonFieldCandidate struct {
	typeOf    reflect.Type
	depth     int
	tagged    bool
	ambiguous bool
}

// Discover fields with encoding/json's promotion rules: shallow fields win,
// then an explicit JSON name wins at the same depth; equal candidates are
// ambiguous and cannot be decoded. Anonymous struct pointers are promoted,
// including exported members of an unexported embedded struct. A json tag
// naming an embedded field instead gives it an ordinary nested object.
func exactJSONFields(target reflect.Type) map[string]reflect.Type {
	if cached, ok := exactFieldCache.Load(target); ok {
		return cached.(map[string]reflect.Type)
	}
	candidates := map[string]jsonFieldCandidate{}
	visited := map[reflect.Type]bool{}
	level := []reflect.Type{target}
	counts := map[reflect.Type]int{target: 1}
	for depth := 1; len(level) != 0; depth++ {
		next := []reflect.Type{}
		nextCounts := map[reflect.Type]int{}
		for _, current := range level {
			if visited[current] {
				continue
			}
			visited[current] = true
			for i := 0; i < current.NumField(); i++ {
				field := current.Field(i)
				child := field.Type
				if child.Kind() == reflect.Pointer {
					child = child.Elem()
				}
				if !field.IsExported() && (!field.Anonymous || child.Kind() != reflect.Struct) {
					continue
				}
				tag := field.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if !validJSONFieldTag(name) {
					name = ""
				}
				if name == "" && field.Anonymous && child.Kind() == reflect.Struct {
					nextCounts[child]++
					if nextCounts[child] == 1 {
						next = append(next, child)
					}
					continue
				}
				candidate := jsonFieldCandidate{typeOf: field.Type, depth: depth, tagged: name != "", ambiguous: counts[current] > 1}
				if name == "" {
					name = field.Name
				}
				old, exists := candidates[name]
				switch {
				case !exists || depth < old.depth || depth == old.depth && candidate.tagged && !old.tagged:
					candidates[name] = candidate
				case depth == old.depth && candidate.tagged == old.tagged:
					old.ambiguous = true
					candidates[name] = old
				}
			}
		}
		level, counts = next, nextCounts
	}
	fields := make(map[string]reflect.Type, len(candidates))
	for name, candidate := range candidates {
		if !candidate.ambiguous {
			fields[name] = candidate.typeOf
		}
	}
	actual, _ := exactFieldCache.LoadOrStore(target, fields)
	return actual.(map[string]reflect.Type)
}

func validJSONFieldTag(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) {
			return false
		}
	}
	return true
}
