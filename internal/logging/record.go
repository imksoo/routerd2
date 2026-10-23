package logging

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var moduleName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
var epochName = regexp.MustCompile(`^[a-f0-9]{32}$`)

func newEpoch() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func levelNumber(level string) (int, bool) {
	switch level {
	case "DEBUG":
		return -4, true
	case "INFO":
		return 0, true
	case "WARN":
		return 4, true
	case "ERROR":
		return 8, true
	default:
		return 0, false
	}
}

func boundedString(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	limit -= len(" [truncated]")
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + " [truncated]"
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, part := range strings.FieldsFunc(key, func(r rune) bool { return r == '.' || r == '/' }) {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "-", "_"), " ", "_")
		if strings.Contains(part, "password") || strings.Contains(part, "passphrase") || strings.Contains(part, "secret") || strings.Contains(part, "token") || strings.Contains(part, "private_key") || strings.Contains(part, "credential") {
			return true
		}
		switch part {
		case "authorization", "config", "configuration", "running_config", "candidate", "argv", "args", "command_line":
			return true
		}
	}
	return false
}

// Only scalar attribute values are accepted. In particular, passing a config
// struct, []byte, or a custom JSON marshaler cannot silently export its contents.
func scalarAttribute(value any) any {
	switch v := value.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "[non-finite number]"
		}
		return v
	case float32:
		return scalarAttribute(float64(v))
	case string:
		return boundedString(v, 1024)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case time.Duration:
		return v.String()
	case error:
		return errorText(v)
	default:
		return "[omitted " + reflect.TypeOf(value).String() + "]"
	}
}

func errorText(err error) (text string) {
	defer func() {
		if recover() != nil {
			text = "[error formatter panicked]"
		}
	}()
	return boundedString(err.Error(), 1024)
}

func cleanAttributes(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	attrs := make(map[string]any, min(len(source), 33))
	for key, value := range source {
		if len(attrs) == 32 {
			attrs["attributes_truncated"] = true
			break
		}
		redact := sensitiveKey(key)
		key = boundedString(key, 128)
		if redact {
			attrs[key] = "[redacted]"
		} else {
			attrs[key] = scalarAttribute(value)
		}
	}
	return attrs
}

func validateEntry(entry Entry) (Entry, error) {
	if !moduleName.MatchString(entry.Module) || entry.PID <= 0 || !epochName.MatchString(entry.ProducerEpoch) || entry.ProducerSequence == 0 {
		return Entry{}, errors.New("log entry has invalid producer identity")
	}
	if _, ok := levelNumber(entry.Level); !ok {
		return Entry{}, errors.New("log level must be DEBUG, INFO, WARN or ERROR")
	}
	if len(entry.Message) > 4096 || !utf8.ValidString(entry.Message) {
		return Entry{}, errors.New("log message exceeds 4096 UTF-8 bytes")
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	} else {
		entry.Timestamp = entry.Timestamp.UTC()
	}
	entry.Attributes = cleanAttributes(entry.Attributes)
	b, err := json.Marshal(entry)
	if err != nil || len(b) > MaxEntryBytes {
		return Entry{}, fmt.Errorf("log entry exceeds %d bytes or is not encodable", MaxEntryBytes)
	}
	return entry, nil
}

func copyRecord(record Record) Record {
	if record.Attributes != nil {
		attrs := make(map[string]any, len(record.Attributes))
		for key, value := range record.Attributes {
			attrs[key] = value // Attributes have already been restricted to scalars.
		}
		record.Attributes = attrs
	}
	return record
}
