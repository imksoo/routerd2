package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"golang.org/x/term"
)

const (
	interactiveDocumentLimit = 100 << 10
	// x/term silently caps a line at 4096 runes. Reject at the byte boundary
	// before reaching that cap, so a large paste can never apply truncated text.
	interactiveLineLimit = 4096
)

var errInputLimit = errors.New("interactive input exceeds its size limit")
var errPasteAborted = errors.New("YAML edit aborted")

type configurationClient interface {
	Do(context.Context, string, string, any, any) error
}

type candidate struct {
	base, document config.Document
	revision       uint64
	path           string
	pendingID      string
	pendingHash    string
}

type interactiveShell struct {
	session         Session
	client          configurationClient
	candidate       *candidate
	confirmRevision uint64
	confirmID       string
	completionOut   io.Writer
}

// Interactive provides a bounded typed terminal and a private candidate per SSH
// session. Disconnecting discards only uncommitted changes. A committed confirmed
// transaction remains owned by the control service's rollback timer.
func (s Session) Interactive(ctx context.Context, rw io.ReadWriter) error {
	input := &terminalInput{reader: bufio.NewReader(rw), writer: rw}
	terminal := term.NewTerminal(input, "routerd> ")
	shell := &interactiveShell{session: s, client: localapi.NewClient(filepath.Join(s.RuntimeDir, control.SocketName)), completionOut: terminal}
	terminal.AutoCompleteCallback = shell.complete
	done := make(chan struct{})
	defer close(done)
	if closer, ok := rw.(io.Closer); ok {
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-done:
			}
		}()
	}
	if _, err := fmt.Fprintln(terminal, "routerd management CLI. Type help; Tab completes commands and configuration paths."); err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		terminal.SetPrompt(shell.prompt())
		line, err := terminal.ReadLine()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !errors.Is(err, term.ErrPasteIndicator) {
			return err
		}
		exit, err := shell.execute(ctx, line, terminal, func() (string, error) {
			return readYAMLPaste(ctx, terminal)
		})
		if errors.Is(err, errInputLimit) {
			_, _ = fmt.Fprintln(terminal, "Error:", err)
			return err // Do not interpret the unconsumed tail of an oversized paste.
		}
		if err != nil {
			if _, writeErr := fmt.Fprintln(terminal, "Error:", err); writeErr != nil {
				return writeErr
			}
		}
		if exit {
			return nil
		}
	}
}

func (s *interactiveShell) prompt() string {
	if s.candidate == nil {
		return "routerd> "
	}
	if s.candidate.path == "" {
		return "routerd(config)# "
	}
	return "routerd(config:" + s.candidate.path + ")# "
}

func (s *interactiveShell) execute(ctx context.Context, line string, out io.Writer, paste func() (string, error)) (bool, error) {
	if !utf8.ValidString(line) || len(line) > interactiveLineLimit {
		return false, errInputLimit
	}
	command, rest := firstWord(line)
	if command == "" {
		return false, nil
	}
	if command == "help" {
		_, err := fmt.Fprint(out, interactiveHelp)
		return false, err
	}
	if command == "configure" && rest == "" {
		if s.candidate != nil {
			return false, errors.New("already configuring; use commit or abort before loading another snapshot")
		}
		current, err := readConfiguration(ctx, s.client, true)
		if err != nil {
			return false, err
		}
		doc, err := cloneDocument(current.Config)
		if err != nil {
			return false, err
		}
		if err := checkCandidateSize(doc); err != nil {
			return false, err
		}
		s.candidate = &candidate{base: current.Config, document: doc, revision: current.Revision}
		_, err = fmt.Fprintf(out, "Editing candidate based on revision %d. Changes apply only on commit.\n", current.Revision)
		return false, err
	}
	if command == "confirm" && rest == "" {
		return false, s.confirm(ctx, out)
	}
	if command == "show" && rest == "running-config" {
		current, err := readConfiguration(ctx, s.client, true)
		if err != nil {
			return false, err
		}
		return false, writeYAML(out, current.Config)
	}
	if command == "exit" && rest == "" {
		if s.candidate == nil {
			return true, nil
		}
		if s.candidate.path != "" {
			s.candidate.path = parentPath(s.candidate.path)
			return false, nil
		}
		if documentsDiffer(s.candidate.base, s.candidate.document) {
			return false, errors.New("candidate has uncommitted changes; use commit or abort")
		}
		s.candidate = nil
		return false, nil
	}
	if s.candidate == nil {
		return false, s.session.Execute(ctx, strings.TrimSpace(line), nil, out)
	}
	c := s.candidate
	switch command {
	case "abort":
		if rest != "" {
			return false, errors.New("usage: abort")
		}
		s.candidate = nil
		_, err := fmt.Fprintln(out, "Candidate discarded.")
		return false, err
	case "top":
		if rest != "" {
			return false, errors.New("usage: top")
		}
		c.path = ""
		return false, nil
	case "up":
		if rest != "" {
			return false, errors.New("usage: up")
		}
		c.path = parentPath(c.path)
		return false, nil
	case "set":
		path, value := firstWord(rest)
		if path == "" || value == "" {
			return false, errors.New("usage: set <path> <JSON value>")
		}
		return false, c.mutate(path, value, false)
	case "delete":
		if rest == "" || strings.ContainsAny(rest, " \t") {
			return false, errors.New("usage: delete <path>")
		}
		return false, c.mutate(rest, "", true)
	case "edit":
		if rest == "running-config.yaml" {
			if paste == nil {
				return false, errors.New("multiline editor is unavailable")
			}
			_, _ = fmt.Fprintln(out, "Paste the complete YAML candidate; .end accepts it, .abort cancels. Limit: 100 KiB, 4 KiB per line.")
			text, err := paste()
			if errors.Is(err, errPasteAborted) {
				_, err = fmt.Fprintln(out, "YAML edit canceled; candidate unchanged.")
				return false, err
			}
			if err != nil {
				return false, err
			}
			if len(text) > interactiveDocumentLimit || !utf8.ValidString(text) {
				return false, errInputLimit
			}
			doc, err := config.Parse([]byte(text))
			if err != nil {
				return false, fmt.Errorf("candidate unchanged: %w", err)
			}
			if err := checkCandidateSize(doc); err != nil {
				return false, err
			}
			c.document, c.path = doc, ""
			_, err = fmt.Fprintln(out, "YAML candidate replaced. Use diff, validate, then commit.")
			return false, err
		}
		if rest == "/" {
			c.path = ""
			return false, nil
		}
		if rest == ".." {
			c.path = parentPath(c.path)
			return false, nil
		}
		path := c.absolutePath(rest)
		parts, err := parsePath(path)
		if err != nil {
			return false, err
		}
		typ, err := pathType(reflect.TypeOf(config.Document{}), parts)
		if err != nil {
			return false, err
		}
		if dereferenceType(typ).Kind() != reflect.Struct {
			return false, errors.New("edit context must be an object; select a named or numbered list entry")
		}
		c.path = path
		return false, nil
	case "show":
		if rest == "" || rest == "candidate" {
			return false, writeYAML(out, c.document)
		}
		if rest == "observations" || strings.HasPrefix(rest, "observations ") {
			return false, s.session.Execute(ctx, "show "+rest, nil, out)
		}
		if rest == "status" || rest == "network" || strings.HasPrefix(rest, "network ") || rest == "version" || rest == "config" || rest == "services" || strings.HasPrefix(rest, "services ") || rest == "route-issues" || strings.HasPrefix(rest, "route-issues ") || rest == "logging" || rest == "running-config" || rest == "logs" || strings.HasPrefix(rest, "logs ") {
			return false, s.session.Execute(ctx, "show "+rest, nil, out)
		}
		parts, err := parsePath(c.absolutePath(rest))
		if err != nil {
			return false, err
		}
		value, err := readPath(reflect.ValueOf(c.document), parts)
		if err != nil {
			return false, err
		}
		return false, WriteJSON(out, value.Interface())
	case "diff":
		if rest != "" {
			return false, errors.New("usage: diff")
		}
		return false, writeCandidateDiff(out, c)
	case "validate":
		if rest != "" {
			return false, errors.New("usage: validate")
		}
		if err := config.Validate(c.document); err != nil {
			return false, err
		}
		_, err := fmt.Fprintln(out, "Candidate configuration is valid.")
		return false, err
	case "commit":
		seconds := 0
		if rest != "" {
			words := strings.Fields(rest)
			if len(words) != 2 || words[0] != "confirmed" {
				return false, errors.New("usage: commit [confirmed <seconds>]")
			}
			var err error
			seconds, err = strconv.Atoi(words[1])
			if err != nil || seconds < 5 || seconds > 3600 {
				return false, errors.New("confirmation timeout must be 5..3600 seconds")
			}
		}
		return false, s.commit(ctx, seconds, out)
	default:
		return false, s.session.Execute(ctx, strings.TrimSpace(line), nil, out)
	}
}

func (s *interactiveShell) commit(ctx context.Context, seconds int, out io.Writer) error {
	c := s.candidate
	if err := config.Validate(c.document); err != nil {
		return err
	}
	encoded, err := json.Marshal(c.document)
	if err != nil {
		return err
	}
	hash := strconv.Itoa(seconds) + ":" + string(encoded)
	if c.pendingID == "" || c.pendingHash != hash {
		c.pendingID, err = requestID()
		if err != nil {
			return err
		}
		c.pendingHash = hash
	}
	request := control.CommitRequest{ExpectedRevision: c.revision, RequestID: c.pendingID, Config: c.document, ConfirmTimeoutSeconds: seconds}
	result, err := commitConfiguration(ctx, s.client, request)
	if err != nil {
		return fmt.Errorf("candidate retained; retry the unchanged commit to reuse request %s: %w", c.pendingID, err)
	}
	doc, err := cloneDocument(result.Config)
	if err != nil {
		return err
	}
	c.base, c.document, c.revision = result.Config, doc, result.Revision
	c.pendingID, c.pendingHash = "", ""
	if result.Confirmation != nil {
		s.confirmRevision, s.confirmID = result.Revision, result.Confirmation.RequestID
		_, err = fmt.Fprintf(out, "Revision %d applied; run confirm within %d seconds or it will roll back.\n", result.Revision, seconds)
	} else {
		s.confirmRevision, s.confirmID = 0, ""
		_, err = fmt.Fprintf(out, "Committed revision %d.\n", result.Revision)
	}
	return err
}

func (s *interactiveShell) confirm(ctx context.Context, out io.Writer) error {
	if s.confirmRevision == 0 {
		current, err := readConfiguration(ctx, s.client, false)
		if err != nil {
			return err
		}
		if current.Confirmation == nil {
			return errors.New("no revision is awaiting confirmation")
		}
		s.confirmRevision, s.confirmID = current.Revision, current.Confirmation.RequestID
	}
	request := control.ConfirmRequest{Revision: s.confirmRevision, RequestID: s.confirmID}
	if _, err := confirmConfiguration(ctx, s.client, request); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "Confirmed revision %d.\n", s.confirmRevision)
	s.confirmRevision, s.confirmID = 0, ""
	return err
}

func requestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return "cli-" + hex.EncodeToString(id[:]), nil
}

func firstWord(line string) (string, string) {
	line = strings.TrimSpace(line)
	if index := strings.IndexAny(line, " \t"); index >= 0 {
		return line[:index], strings.TrimSpace(line[index+1:])
	}
	return line, ""
}

func cloneDocument(doc config.Document) (config.Document, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return config.Document{}, err
	}
	var copy config.Document
	err = json.Unmarshal(b, &copy)
	return copy, err
}

func documentsDiffer(a, b config.Document) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return !bytes.Equal(left, right)
}

func checkCandidateSize(doc config.Document) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if len(b) > interactiveDocumentLimit {
		return errors.New("candidate exceeds the interactive 100 KiB limit; use the configuration API for larger documents")
	}
	return nil
}

// Paths address only exported fields from the configuration schema. A named
// list selector is a key, never a file name or executable command.
type pathPart struct {
	field, selector string
	selected        bool
}

func parsePath(path string) ([]pathPart, error) {
	if path == "" || len(path) > 1024 {
		return nil, errors.New("configuration path must contain 1..1024 bytes")
	}
	var parts []pathPart
	for i := 0; i < len(path); {
		start := i
		for i < len(path) && ((path[i] >= 'a' && path[i] <= 'z') || (path[i] >= '0' && path[i] <= '9') || path[i] == '_') {
			i++
		}
		if i == start {
			return nil, fmt.Errorf("invalid configuration path %q", path)
		}
		part := pathPart{field: path[start:i]}
		if i < len(path) && path[i] == '[' {
			part.selected = true
			i++
			start = i
			for i < len(path) && path[i] != ']' {
				ch := path[i]
				if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("_.:/-", rune(ch))) {
					return nil, errors.New("list selectors allow letters, digits, underscore, dot, colon, slash and hyphen")
				}
				i++
			}
			if i == start || i == len(path) {
				return nil, errors.New("list selector must be nonempty and end with ]")
			}
			part.selector = path[start:i]
			i++
		}
		parts = append(parts, part)
		if len(parts) > 32 {
			return nil, errors.New("configuration path is too deep")
		}
		if i < len(path) {
			if path[i] != '.' || i == len(path)-1 {
				return nil, fmt.Errorf("invalid configuration path %q", path)
			}
			i++
		}
	}
	return parts, nil
}

func (c *candidate) absolutePath(path string) string {
	if strings.HasPrefix(path, "/") {
		return strings.TrimPrefix(path, "/")
	}
	if c.path == "" {
		return path
	}
	if path == "" {
		return c.path
	}
	return c.path + "." + path
}

func parentPath(path string) string {
	parts, err := parsePath(path)
	if err != nil || len(parts) <= 1 {
		return ""
	}
	var names []string
	for _, part := range parts[:len(parts)-1] {
		name := part.field
		if part.selected {
			name += "[" + part.selector + "]"
		}
		names = append(names, name)
	}
	return strings.Join(names, ".")
}

func dereferenceType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func jsonField(t reflect.Type, name string) (int, error) {
	t = dereferenceType(t)
	if t.Kind() != reflect.Struct {
		return 0, errors.New("path continues through a non-object value")
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.IsExported() && strings.Split(field.Tag.Get("json"), ",")[0] == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown configuration field %q", name)
}

func namedList(t reflect.Type) (int, bool) {
	for _, key := range []string{"id", "name"} {
		index, err := jsonField(t, key)
		if err == nil && dereferenceType(t).Field(index).Type.Kind() == reflect.String {
			return index, true
		}
	}
	return 0, false
}

func pathType(t reflect.Type, parts []pathPart) (reflect.Type, error) {
	for _, part := range parts {
		t = dereferenceType(t)
		index, err := jsonField(t, part.field)
		if err != nil {
			return nil, err
		}
		t = t.Field(index).Type
		if part.selected {
			if t.Kind() != reflect.Slice {
				return nil, fmt.Errorf("%s is not a list", part.field)
			}
			t = t.Elem()
			if _, named := namedList(t); !named {
				if _, err := listNumber(part.selector); err != nil {
					return nil, err
				}
			}
		}
	}
	return t, nil
}

func listNumber(selector string) (int, error) {
	n, err := strconv.Atoi(selector)
	if err != nil || n < 0 || strconv.Itoa(n) != selector {
		return 0, errors.New("this list needs a nonnegative numeric selector, such as [0]")
	}
	return n, nil
}

func listIndex(v reflect.Value, selector string, create bool) (int, error) {
	if key, named := namedList(v.Type().Elem()); named {
		for i := 0; i < v.Len(); i++ {
			if v.Index(i).Field(key).String() == selector {
				return i, nil
			}
		}
		if !create {
			return 0, fmt.Errorf("list entry %q is not configured", selector)
		}
		entry := reflect.New(v.Type().Elem()).Elem()
		entry.Field(key).SetString(selector)
		v.Set(reflect.Append(v, entry))
		return v.Len() - 1, nil
	}
	index, err := listNumber(selector)
	if err != nil {
		return 0, err
	}
	if index < v.Len() {
		return index, nil
	}
	if create && index == v.Len() {
		v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		return index, nil
	}
	return 0, fmt.Errorf("list index %d is out of range (length %d)", index, v.Len())
}

func (c *candidate) mutate(path, value string, remove bool) error {
	parts, err := parsePath(c.absolutePath(path))
	if err != nil {
		return err
	}
	if _, err = pathType(reflect.TypeOf(c.document), parts); err != nil {
		return err
	}
	copy, err := cloneDocument(c.document)
	if err != nil {
		return err
	}
	if remove && len(parts) == 1 && parts[0].field == "resources" && parts[0].selected {
		copy, _, err = resourceprogram.RemoveOwned(copy, parts[0].selector)
	} else {
		err = mutatePath(reflect.ValueOf(&copy).Elem(), parts, value, remove)
	}
	if err != nil {
		return err
	}
	if err = checkCandidateSize(copy); err != nil {
		return err
	}
	c.document = copy
	return nil
}

func mutatePath(v reflect.Value, parts []pathPart, value string, remove bool) error {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			if remove {
				return errors.New("parent configuration is not set")
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	part := parts[0]
	index, err := jsonField(v.Type(), part.field)
	if err != nil {
		return err
	}
	field := v.Field(index)
	target := field
	selected := -1
	if part.selected {
		selected, err = listIndex(field, part.selector, !remove)
		if err != nil {
			return err
		}
		target = field.Index(selected)
	}
	if len(parts) > 1 {
		return mutatePath(target, parts[1:], value, remove)
	}
	if remove {
		if selected >= 0 {
			field.Set(reflect.AppendSlice(field.Slice(0, selected), field.Slice(selected+1, field.Len())))
		} else {
			target.SetZero()
		}
		return nil
	}
	if !json.Valid([]byte(value)) {
		return errors.New("set value must be valid JSON; quote strings and object keys")
	}
	if strings.TrimSpace(value) == "null" && target.Kind() != reflect.Pointer && target.Kind() != reflect.Slice {
		return errors.New("this field is not nullable; use delete to unset it")
	}
	decoded := reflect.New(target.Type())
	// The shared decoder rejects unknown and duplicate object fields. JSON is
	// checked above so interactive set never guesses a bare YAML scalar's type.
	if err := config.Decode(strings.NewReader(value), decoded.Interface()); err != nil {
		return fmt.Errorf("invalid value for %s: %w", part.field, err)
	}
	if selected >= 0 {
		if key, named := namedList(target.Type()); named {
			name := decoded.Elem().Field(key)
			if name.String() != "" && name.String() != part.selector {
				return errors.New("entry identity must match its list selector")
			}
			name.SetString(part.selector)
		}
	}
	target.Set(decoded.Elem())
	return nil
}

func readPath(v reflect.Value, parts []pathPart) (reflect.Value, error) {
	for _, part := range parts {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, errors.New("configuration is not set")
			}
			v = v.Elem()
		}
		index, err := jsonField(v.Type(), part.field)
		if err != nil {
			return reflect.Value{}, err
		}
		v = v.Field(index)
		if part.selected {
			if v.Kind() != reflect.Slice {
				return reflect.Value{}, fmt.Errorf("%s is not a list", part.field)
			}
			index, err = listIndex(v, part.selector, false)
			if err != nil {
				return reflect.Value{}, err
			}
			v = v.Index(index)
		}
	}
	return v, nil
}

func writeCandidateDiff(out io.Writer, c *candidate) error {
	before, err := config.YAML(c.base)
	if err != nil {
		return err
	}
	after, err := config.YAML(c.document)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		_, err = fmt.Fprintln(out, "No changes.")
		return err
	}
	oldLines := strings.Split(strings.TrimSuffix(string(before), "\n"), "\n")
	newLines := strings.Split(strings.TrimSuffix(string(after), "\n"), "\n")
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-suffix-1] == newLines[len(newLines)-suffix-1] {
		suffix++
	}
	start := max(0, prefix-3)
	contextAfter := min(3, suffix)
	oldEnd, newEnd := len(oldLines)-suffix, len(newLines)-suffix
	var b strings.Builder
	fmt.Fprintf(&b, "--- running-config@revision-%d\n+++ candidate\n@@ -%d,%d +%d,%d @@\n", c.revision, start+1, oldEnd+contextAfter-start, start+1, newEnd+contextAfter-start)
	for _, line := range oldLines[start:prefix] {
		fmt.Fprintln(&b, " "+line)
	}
	for _, line := range oldLines[prefix:oldEnd] {
		fmt.Fprintln(&b, "-"+line)
	}
	for _, line := range newLines[prefix:newEnd] {
		fmt.Fprintln(&b, "+"+line)
	}
	for _, line := range oldLines[oldEnd : oldEnd+contextAfter] {
		fmt.Fprintln(&b, " "+line)
	}
	_, err = io.WriteString(out, b.String())
	return err
}

func (s *interactiveShell) complete(line string, pos int, key rune) (string, int, bool) {
	if key != '\t' || pos < 0 || pos > len(line) || !utf8.ValidString(line) || !utf8.ValidString(line[:pos]) {
		return "", 0, false
	}
	start := strings.LastIndexAny(line[:pos], " \t") + 1
	token := line[start:pos]
	words := strings.Fields(line[:start])
	var choices []string
	switch len(words) {
	case 0:
		choices = []string{"help", "show", "configure", "confirm", "restart", "exit"}
		if s.candidate != nil {
			choices = append(choices, "set", "delete", "edit", "up", "top", "diff", "validate", "commit", "abort")
		}
	case 1:
		switch words[0] {
		case "show":
			choices = []string{"status", "network", "resources", "observations", "services", "route-issues", "logging", "logs", "version", "config", "running-config"}
			if s.candidate != nil {
				choices = append(choices, "candidate")
				choices = append(choices, s.pathChoices(token)...)
			}
		case "set", "delete", "edit":
			if s.candidate != nil {
				choices = s.pathChoices(token)
				if words[0] == "edit" {
					choices = append(choices, "running-config.yaml", "/", "..")
				}
			}
		case "commit":
			if s.candidate != nil {
				choices = []string{"confirmed"}
			}
		}
	}
	if len(words) >= 2 && words[0] == "show" && words[1] == "network" {
		choices = networkCompletions(words)
	}
	if len(words) >= 2 && len(words)%2 == 0 && words[0] == "show" && words[1] == "observations" {
		choices = []string{"after", "limit"}
		for i := 2; i < len(words); i += 2 {
			if words[i] == "after" {
				choices = slices.DeleteFunc(choices, func(v string) bool { return v == "after" })
			}
			if words[i] == "limit" {
				choices = slices.DeleteFunc(choices, func(v string) bool { return v == "limit" })
			}
		}
	}
	var matches []string
	for _, choice := range choices {
		if strings.HasPrefix(choice, token) {
			matches = append(matches, choice)
		}
	}
	if len(matches) == 0 {
		return line, pos, true // Consume Tab without inserting control characters.
	}
	sort.Strings(matches)
	common := matches[0]
	for _, match := range matches[1:] {
		for !strings.HasPrefix(match, common) {
			_, size := utf8.DecodeLastRuneInString(common)
			common = common[:len(common)-size]
		}
	}
	if len(matches) == 1 {
		if pos == len(line) || (line[pos] != ' ' && line[pos] != '\t') {
			common += " "
		}
	} else if common == token && s.completionOut != nil {
		visible := matches[:min(len(matches), 20)]
		_, _ = fmt.Fprintln(s.completionOut, "\n"+strings.Join(visible, "  "))
		if len(matches) > len(visible) {
			_, _ = fmt.Fprintf(s.completionOut, "(%d additional choices; type a longer prefix)\n", len(matches)-len(visible))
		}
	}
	completed := line[:start] + common + line[pos:]
	if len(completed) > interactiveLineLimit {
		return line, pos, true
	}
	return completed, start + len(common), true
}

func (s *interactiveShell) pathChoices(token string) []string {
	var paths []string
	target := strings.TrimPrefix(token, "/")
	if !strings.HasPrefix(token, "/") && s.candidate.path != "" {
		target = s.candidate.path + "." + token
	}
	collectPaths(reflect.TypeOf(s.candidate.document), reflect.ValueOf(s.candidate.document), "", 0, &paths, target)
	var choices []string
	for _, path := range paths {
		if strings.HasPrefix(token, "/") {
			choices = append(choices, "/"+path)
		} else if s.candidate.path == "" {
			choices = append(choices, path)
		} else if strings.HasPrefix(path, s.candidate.path+".") {
			choices = append(choices, strings.TrimPrefix(path, s.candidate.path+"."))
		}
	}
	return choices
}

func collectPaths(t reflect.Type, v reflect.Value, prefix string, depth int, out *[]string, target string) {
	if depth > 8 || len(*out) >= 2000 {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
		if v.IsValid() && !v.IsNil() {
			v = v.Elem()
		} else {
			v = reflect.Zero(t)
		}
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField() && len(*out) < 2000; i++ {
		field := t.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if !field.IsExported() || name == "" || name == "-" {
			continue
		}
		path := prefix + name
		if !strings.HasPrefix(target, path) && !strings.HasPrefix(path, target) {
			continue
		}
		*out = append(*out, path)
		value := reflect.Zero(field.Type)
		if v.IsValid() {
			value = v.Field(i)
		}
		if field.Type.Kind() != reflect.Slice {
			collectPaths(field.Type, value, path+".", depth+1, out, target)
			continue
		}
		key, named := namedList(field.Type.Elem())
		for j := 0; j < value.Len() && len(*out) < 2000; j++ {
			selector := strconv.Itoa(j)
			if named {
				selector = value.Index(j).Field(key).String()
			}
			entry := path + "[" + selector + "]"
			if !strings.HasPrefix(target, entry) && !strings.HasPrefix(entry, target) {
				continue
			}
			if _, err := parsePath(entry); err != nil {
				continue
			}
			*out = append(*out, entry)
			collectPaths(field.Type.Elem(), value.Index(j), entry+".", depth+1, out, target)
		}
		if !named && len(*out) < 2000 {
			// The first unused index creates a new entry; sparse lists are forbidden.
			entry := path + "[" + strconv.Itoa(value.Len()) + "]"
			*out = append(*out, entry)
			collectPaths(field.Type.Elem(), reflect.Zero(field.Type.Elem()), entry+".", depth+1, out, target)
		}
	}
}

func writeYAML(out io.Writer, doc config.Document) error {
	b, err := config.YAML(doc)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

func readYAMLPaste(ctx context.Context, terminal *term.Terminal) (string, error) {
	callback, history := terminal.AutoCompleteCallback, terminal.History
	terminal.AutoCompleteCallback, terminal.History = nil, discardHistory{}
	defer func() { terminal.AutoCompleteCallback, terminal.History = callback, history }()
	terminal.SetPrompt("yaml> ")
	var content strings.Builder
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		line, err := terminal.ReadLine()
		if err != nil && !errors.Is(err, term.ErrPasteIndicator) {
			return "", err
		}
		if line == ".end" {
			return content.String(), nil
		}
		if line == ".abort" {
			return "", errPasteAborted
		}
		if content.Len()+len(line)+1 > interactiveDocumentLimit {
			return "", errInputLimit
		}
		content.WriteString(line)
		content.WriteByte('\n')
	}
}

type discardHistory struct{}

func (discardHistory) Add(string)    {}
func (discardHistory) Len() int      { return 0 }
func (discardHistory) At(int) string { return "" }

// terminalInput validates UTF-8 before x/term sees it and bounds raw physical
// lines, including a paste that the terminal would otherwise silently truncate.
type terminalInput struct {
	reader  *bufio.Reader
	writer  io.Writer
	pending []byte
	line    int
}

func (r *terminalInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		ch, size, err := r.reader.ReadRune()
		if err != nil {
			return 0, err
		}
		if ch == utf8.RuneError && size == 1 {
			return 0, errors.New("interactive input is not valid UTF-8")
		}
		if ch == '\n' || ch == '\r' {
			r.line = 0
		} else {
			r.line += size
			if r.line > interactiveLineLimit {
				return 0, fmt.Errorf("%w: maximum 4 KiB per line", errInputLimit)
			}
		}
		r.pending = []byte(string(ch))
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *terminalInput) Write(p []byte) (int, error) { return r.writer.Write(p) }

const interactiveHelp = `Operational commands:
  show status | network | version | config | running-config
  show network help           list paged network observation commands
  show observations [after CURSOR] [limit 1..16]
  restart <module>
  configure                  start a private candidate from the current revision
  confirm                    confirm a pending confirmed commit
  exit
Configuration commands:
  set <path> <JSON value>     set a typed field; use JSON quotes for strings
  delete <path>              unset a field or remove a list entry
  edit <object path>         enter a relative configuration context
  up | top                   move to a parent context or the root
  show [candidate | path]    inspect the candidate
  diff | validate
  commit [confirmed 60]       revision-checked commit; seconds range 5..3600
  edit running-config.yaml   replace candidate with a multiline YAML paste
  abort                      discard the candidate and leave configuration mode
  exit                       leave context, then configuration mode if unchanged
Examples:
  set resources[lan-address].address.cidr "192.0.2.1/24"
  set resources[forwarding].forwarding.ipv4 true
  edit resources[lan].interface
  set mtu 1500
Resources use [id]; other lists use [name] when present, otherwise [0], [1], etc.
Prefix a path with / to address it from the configuration root.
YAML export is canonical; comments and original formatting are not retained.
Input limits: 4 KiB per line and 100 KiB per interactive candidate/YAML paste.
`
