package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"golang.org/x/term"
)

// fakeConfigurationClient models the control API's revision and request-ID
// contract, including an applied transaction whose response was lost.
type fakeConfigurationClient struct {
	state              control.State
	commits            []control.CommitRequest
	confirms           []control.ConfirmRequest
	loseCommitResponse bool
}

func (f *fakeConfigurationClient) Do(_ context.Context, method, path string, in, out any) error {
	switch {
	case method == http.MethodGet && path == "/v1/config":
		return jsonCopy(out, f.state)
	case method == http.MethodPost && path == "/v1/config/commit":
		var r control.CommitRequest
		if err := jsonCopy(&r, in); err != nil {
			return err
		}
		f.commits = append(f.commits, r)
		if r.RequestID == f.state.LastRequestID {
			if documentsDiffer(r.Config, f.state.Config) {
				return errors.New("request ID content mismatch")
			}
			return jsonCopy(out, f.state)
		}
		if r.ExpectedRevision != f.state.Revision {
			return errors.New("HTTP 409: revision conflict")
		}
		if f.state.Confirmation != nil {
			return errors.New("HTTP 409: pending confirmation")
		}
		previous := f.state.Committed
		f.state.Committed = config.Committed{Revision: previous.Revision + 1, Config: r.Config, LastRequestID: r.RequestID}
		if r.ConfirmTimeoutSeconds > 0 {
			f.state.Confirmation = &config.Confirmation{Previous: previous.Config, PreviousRevision: previous.Revision, RequestID: r.RequestID, Deadline: time.Now().Add(time.Duration(r.ConfirmTimeoutSeconds) * time.Second)}
		}
		if f.loseCommitResponse {
			f.loseCommitResponse = false
			return errors.New("connection closed before response")
		}
		return jsonCopy(out, f.state)
	case method == http.MethodPost && path == "/v1/config/confirm":
		var r control.ConfirmRequest
		if err := jsonCopy(&r, in); err != nil {
			return err
		}
		f.confirms = append(f.confirms, r)
		if r.Revision != f.state.Revision || r.RequestID != f.state.LastRequestID {
			return errors.New("HTTP 409: wrong confirmation identity")
		}
		f.state.Confirmation = nil
		return jsonCopy(out, f.state)
	default:
		return errors.New("unexpected API request: " + method + " " + path)
	}
}

func jsonCopy(out, in any) error {
	if out == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func fixtureShell() (*interactiveShell, *fakeConfigurationClient) {
	d := config.Document{SchemaVersion: 1, Network: network.Desired{Interfaces: []network.Interface{{Name: "eth0", MTU: 1500, Addresses: []string{"192.0.2.1/24"}}}}}
	f := &fakeConfigurationClient{state: control.State{Committed: config.Committed{Revision: 4, Config: d}, Healthy: true}}
	return &interactiveShell{client: f}, f
}

func command(t *testing.T, shell *interactiveShell, line string) string {
	t.Helper()
	var out bytes.Buffer
	exit, err := shell.execute(context.Background(), line, &out, nil)
	if err != nil || exit {
		t.Fatalf("command %q: exit=%v error=%v output=%s", line, exit, err, out.String())
	}
	return out.String()
}

func commandError(t *testing.T, shell *interactiveShell, line string) error {
	t.Helper()
	_, err := shell.execute(context.Background(), line, io.Discard, nil)
	if err == nil {
		t.Fatalf("command %q unexpectedly succeeded", line)
	}
	return err
}

func TestCandidateIsolationNamedPathsAndCommit(t *testing.T) {
	s, f := fixtureShell()
	command(t, s, "configure")
	command(t, s, `set network.interfaces[eth0].addresses ["192.0.2.5/24","2001:db8::5/64"]`)
	command(t, s, "edit network.interfaces[eth0]")
	command(t, s, "set mtu 1400")
	if s.prompt() != "routerd(config:network.interfaces[eth0])# " {
		t.Fatalf("wrong context prompt: %q", s.prompt())
	}
	command(t, s, "set /network.forwarding.ipv4 true")
	command(t, s, "delete addresses[0]")
	command(t, s, "top")
	command(t, s, `set network.interfaces[eth1] {"mtu":1500,"addresses":["198.51.100.1/24"]}`)
	if f.state.Config.Network.Interfaces[0].MTU != 1500 || len(f.commits) != 0 {
		t.Fatal("candidate edits escaped into the running configuration")
	}
	if s.candidate.base.Network.Interfaces[0].Addresses[0] != "192.0.2.1/24" {
		t.Fatal("candidate changed its base snapshot")
	}
	if got := s.candidate.document.Network.Interfaces[1].Name; got != "eth1" {
		t.Fatalf("named entry did not get its selector key: %q", got)
	}
	commandError(t, s, "exit")
	command(t, s, "validate")
	command(t, s, "commit")
	if f.state.Revision != 5 || s.candidate.revision != 5 || documentsDiffer(s.candidate.base, s.candidate.document) {
		t.Fatal("successful commit did not advance and reset the candidate snapshot")
	}
	if *f.state.Config.Network.Forwarding.IPv4 != true || f.state.Config.Network.Interfaces[0].MTU != 1400 {
		t.Fatal("typed candidate was not submitted")
	}
	command(t, s, "exit")
	if s.candidate != nil {
		t.Fatal("clean exit did not leave configuration mode")
	}
}

func TestTypedMutationRejectsInvalidValuesAtomically(t *testing.T) {
	s, _ := fixtureShell()
	command(t, s, "configure")
	before, _ := cloneDocument(s.candidate.document)
	for _, line := range []string{
		`set network.unknown true`,
		`set network.interfaces[eth0].mtu "1500"`,
		`set network.interfaces[eth0].mtu null`,
		`set network.interfaces[eth1] {"name":"eth2"}`,
		`set network.interfaces[eth1] {"unknown":3}`,
		`set network.interfaces[eth1] {"mtu":1400,"mtu":1500}`,
		`set network.firewall.forward[0].from true`,
		`set network.routes[2].interface "eth0"`,
		`set network.interfaces[eth0].addresses[4] "192.0.2.2/24"`,
		`set network.forwarding.ipv4 true false`,
		`delete network.interfaces[missing]`,
	} {
		t.Run(line, func(t *testing.T) {
			commandError(t, s, line)
			if documentsDiffer(before, s.candidate.document) {
				t.Fatal("failed mutation partially changed the candidate")
			}
		})
	}
	command(t, s, `set network.routes[0] {"destination":"0.0.0.0/0","interface":"eth0","gateway":"192.0.2.254"}`)
	command(t, s, `set network.firewall.forward[0] {"from":"eth0","to":"eth1"}`)
	command(t, s, "set network.forwarding.ipv6 false")
	if s.candidate.document.Network.Firewall == nil || s.candidate.document.Network.Forwarding.IPv6 == nil || *s.candidate.document.Network.Forwarding.IPv6 {
		t.Fatal("pointer and list values were not initialized correctly")
	}
	command(t, s, "delete network.forwarding.ipv6")
	command(t, s, "delete network.routes[0]")
	if s.candidate.document.Network.Forwarding.IPv6 != nil || len(s.candidate.document.Network.Routes) != 0 {
		t.Fatal("delete did not restore the unset representation")
	}
}

func TestInvalidIntermediateCandidateCannotCommit(t *testing.T) {
	s, f := fixtureShell()
	command(t, s, "configure")
	command(t, s, `set network.interfaces[eth0].addresses ["not-a-prefix"]`)
	if !strings.Contains(command(t, s, "show candidate"), "not-a-prefix") {
		t.Fatal("invalid intermediate candidate must remain inspectable")
	}
	commandError(t, s, "validate")
	commandError(t, s, "commit")
	if len(f.commits) != 0 {
		t.Fatal("invalid candidate reached the write API")
	}
	command(t, s, "abort")
	if s.candidate != nil || f.state.Revision != 4 {
		t.Fatal("abort affected running state")
	}
}

func TestCommitConflictRetainsCandidateAndRevision(t *testing.T) {
	s, f := fixtureShell()
	command(t, s, "configure")
	command(t, s, "set network.interfaces[eth0].mtu 1400")
	f.state.Revision++ // Another administrator commits after this snapshot.
	err := commandError(t, s, "commit")
	if !strings.Contains(err.Error(), "candidate retained") || s.candidate.revision != 4 || s.candidate.document.Network.Interfaces[0].MTU != 1400 {
		t.Fatalf("conflict lost the candidate: %v", err)
	}
	if f.commits[0].ExpectedRevision != 4 || f.state.Config.Network.Interfaces[0].MTU != 1500 {
		t.Fatal("optimistic revision guard was bypassed")
	}
}

func TestLostCommitResponseReusesRequestIdentity(t *testing.T) {
	s, f := fixtureShell()
	command(t, s, "configure")
	command(t, s, "set network.interfaces[eth0].mtu 1400")
	f.loseCommitResponse = true
	commandError(t, s, "commit")
	if f.state.Revision != 5 || s.candidate.revision != 4 {
		t.Fatal("fixture did not model an applied but unacknowledged transaction")
	}
	command(t, s, "commit")
	if len(f.commits) != 2 || f.commits[0].RequestID == "" || f.commits[0].RequestID != f.commits[1].RequestID || f.state.Revision != 5 {
		t.Fatal("retry created a second transaction instead of reusing the first request")
	}
	if s.candidate.revision != 5 {
		t.Fatal("retry did not reconcile the local base revision")
	}
}

func TestConfirmedCommitUsesOriginalTransactionIdentity(t *testing.T) {
	s, f := fixtureShell()
	command(t, s, "configure")
	command(t, s, "set network.interfaces[eth0].mtu 1400")
	commandError(t, s, "commit confirmed 4")
	commandError(t, s, "commit confirmed 3601")
	command(t, s, "commit confirmed 60")
	if f.state.Confirmation == nil || f.commits[0].ConfirmTimeoutSeconds != 60 {
		t.Fatal("confirmed commit did not preserve its timeout")
	}
	command(t, s, "confirm")
	if f.state.Confirmation != nil || f.confirms[0].Revision != 5 || f.confirms[0].RequestID != f.commits[0].RequestID {
		t.Fatal("confirmation did not identify the committed transaction")
	}
	commandError(t, s, "confirm")
	command(t, s, "commit confirmed 60")
	// Reconnecting obtains the pending transaction's ID from control, so the
	// operator can recover after the SSH session that performed commit is lost.
	reconnected := &interactiveShell{client: f}
	command(t, reconnected, "confirm")
	if len(f.confirms) != 2 || f.confirms[1].RequestID != f.commits[1].RequestID {
		t.Fatal("new session failed to confirm the persisted transaction")
	}
}

func TestYAMLEditPreservesCandidateOnErrorAndAbort(t *testing.T) {
	s, f := fixtureShell()
	command(t, s, "configure")
	runPaste := func(text string, pasteErr error) error {
		_, err := s.execute(context.Background(), "edit running-config.yaml", io.Discard, func() (string, error) { return text, pasteErr })
		return err
	}
	yamlText := "schema_version: 1\nnetwork:\n  forwarding:\n    ipv4: true\n"
	if err := runPaste(yamlText, nil); err != nil {
		t.Fatal(err)
	}
	accepted, _ := cloneDocument(s.candidate.document)
	for _, text := range []string{"network: [", "schema_version: 1\nnetwork:\n  mystery: true\n"} {
		if err := runPaste(text, nil); err == nil || documentsDiffer(accepted, s.candidate.document) {
			t.Fatalf("invalid paste changed candidate: %q, error=%v", text, err)
		}
	}
	if err := runPaste("", errPasteAborted); err != nil || documentsDiffer(accepted, s.candidate.document) {
		t.Fatal("aborting the editor changed candidate")
	}
	if err := runPaste(strings.Repeat("a", interactiveDocumentLimit+1), nil); !errors.Is(err, errInputLimit) || documentsDiffer(accepted, s.candidate.document) {
		t.Fatal("oversized paste was not rejected atomically")
	}
	if f.state.Config.Network.Forwarding.IPv4 != nil || len(f.commits) != 0 {
		t.Fatal("YAML paste bypassed commit")
	}
}

func TestCandidateSizeLimitIsAtomic(t *testing.T) {
	s, _ := fixtureShell()
	command(t, s, "configure")
	before, _ := cloneDocument(s.candidate.document)
	value, _ := json.Marshal([]string{strings.Repeat("x", interactiveDocumentLimit)})
	if err := s.candidate.mutate("network.interfaces[eth0].addresses", string(value), false); err == nil {
		t.Fatal("oversized candidate accepted")
	}
	if documentsDiffer(before, s.candidate.document) {
		t.Fatal("size rejection partially changed candidate")
	}
}

func TestCandidateDiffAndContextNavigation(t *testing.T) {
	s, _ := fixtureShell()
	command(t, s, "configure")
	if got := command(t, s, "diff"); got != "No changes.\n" {
		t.Fatalf("unexpected empty diff %q", got)
	}
	command(t, s, "edit network.interfaces[eth0]")
	command(t, s, "set mtu 1400")
	if got := command(t, s, "show mtu"); strings.TrimSpace(got) != "1400" {
		t.Fatalf("relative show returned %q", got)
	}
	diff := command(t, s, "diff")
	if !strings.Contains(diff, "--- running-config@revision-4\n+++ candidate\n@@") {
		t.Fatalf("missing unified diff header: %s", diff)
	}
	var removed, added bool
	for _, line := range strings.Split(diff, "\n") {
		removed = removed || strings.HasPrefix(line, "-") && strings.Contains(line, "mtu: 1500")
		added = added || strings.HasPrefix(line, "+") && strings.Contains(line, "mtu: 1400")
	}
	if !removed || !added {
		t.Fatalf("diff omitted changed value: %s", diff)
	}
	command(t, s, "up")
	if s.candidate.path != "network" {
		t.Fatalf("wrong parent context %q", s.candidate.path)
	}
	command(t, s, "exit")
	if s.candidate.path != "" || s.candidate == nil {
		t.Fatal("exit did not leave only the current context")
	}
	commandError(t, s, "edit network.interfaces")
}

func TestCompletionRespectsContextAndUTF8CursorBoundaries(t *testing.T) {
	s, _ := fixtureShell()
	line, pos, ok := s.complete("configu", 7, '\t')
	if !ok || line != "configure " || pos != len(line) {
		t.Fatalf("command completion: %q %d %v", line, pos, ok)
	}
	command(t, s, "configure")
	input := "set network.interfaces[eth0].mt"
	line, pos, ok = s.complete(input, len(input), '\t')
	if !ok || line != "set network.interfaces[eth0].mtu " || pos != len(line) {
		t.Fatalf("path completion: %q %d %v", line, pos, ok)
	}
	command(t, s, "edit network.interfaces[eth0]")
	line, _, _ = s.complete("set mt", len("set mt"), '\t')
	if line != "set mtu " {
		t.Fatalf("relative completion: %q", line)
	}
	line, _, _ = s.complete("set /network.forw", len("set /network.forw"), '\t')
	if line != "set /network.forwarding" {
		t.Fatalf("absolute completion: %q", line)
	}
	for _, pos := range []int{-1, 1, 3} {
		if _, _, ok := s.complete("é", pos, '\t'); ok {
			t.Fatalf("accepted invalid cursor offset %d", pos)
		}
	}
	if _, _, ok := s.complete("set mt", 6, 'x'); ok {
		t.Fatal("completion consumed a normal keystroke")
	}
}

func TestPathGrammarAndReadErrors(t *testing.T) {
	for _, path := range []string{"", ".network", "network.", "network..interfaces", "network.interfaces[]", "network.interfaces[eth0", "network.interfaces[../x]", "network.interfaces[eth 0]", "network.interfaces[eth0]junk"} {
		if _, err := parsePath(path); err == nil {
			t.Errorf("accepted invalid path %q", path)
		}
	}
	parts, err := parsePath("network.interfaces[eth0.100].addresses[0]")
	if err != nil || len(parts) != 3 || parts[1].selector != "eth0.100" {
		t.Fatalf("dot in interface selector split the path: %v %v", parts, err)
	}
	s, _ := fixtureShell()
	command(t, s, "configure")
	for _, path := range []string{"network.firewall.forward", "network.interfaces[absent].mtu", "network.interfaces[eth0].mtu[0]", "network.interfaces[eth0].mtu.value"} {
		parts, _ := parsePath(path)
		if _, err := readPath(reflect.ValueOf(s.candidate.document), parts); err == nil {
			t.Errorf("read of missing/invalid path %q succeeded", path)
		}
	}
}

func TestTerminalInputUTF8AndLineBounds(t *testing.T) {
	text := "set 描述 \"ネットワーク\"\rnext\n"
	input := &terminalInput{reader: bufio.NewReader(strings.NewReader(text)), writer: io.Discard}
	var collected bytes.Buffer
	var one [1]byte
	for {
		n, err := input.Read(one[:])
		collected.Write(one[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if collected.String() != text || !utf8.Valid(collected.Bytes()) {
		t.Fatal("fragmented reads corrupted UTF-8")
	}
	for _, test := range []struct {
		name, text string
		limit      bool
	}{
		{"invalid UTF8", "set \xff\r", false},
		{"long ASCII line", strings.Repeat("a", interactiveLineLimit+1) + "\r", true},
		{"long multibyte line", strings.Repeat("日", interactiveLineLimit/3+1) + "\r", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &terminalInput{reader: bufio.NewReader(strings.NewReader(test.text)), writer: io.Discard}
			_, err := io.ReadAll(r)
			if err == nil || test.limit && !errors.Is(err, errInputLimit) {
				t.Fatalf("input not rejected: %v", err)
			}
		})
	}
	input = &terminalInput{reader: bufio.NewReader(strings.NewReader(strings.Repeat("a", interactiveLineLimit) + "\r" + strings.Repeat("b", interactiveLineLimit) + "\r")), writer: io.Discard}
	if _, err := io.Copy(io.Discard, input); err != nil {
		t.Fatalf("per-line counter was not reset: %v", err)
	}
}

type scriptedTerminal struct {
	input  *strings.Reader
	output bytes.Buffer
}

func (s *scriptedTerminal) Read(p []byte) (int, error)  { return s.input.Read(p) }
func (s *scriptedTerminal) Write(p []byte) (int, error) { return s.output.Write(p) }

func TestYAMLPasteTerminators(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		end := ".end"
		if cancel {
			end = ".abort"
		}
		rw := &scriptedTerminal{input: strings.NewReader("schema_version: 1\rnetwork: {}\r" + end + "\r")}
		terminal := term.NewTerminal(rw, "")
		text, err := readYAMLPaste(context.Background(), terminal)
		if cancel {
			if !errors.Is(err, errPasteAborted) {
				t.Fatalf("abort terminator: %v", err)
			}
		} else if err != nil || text != "schema_version: 1\nnetwork: {}\n" {
			t.Fatalf("end terminator: text=%q error=%v", text, err)
		}
	}
}

func TestInteractiveTerminalAndNoShellEscape(t *testing.T) {
	rw := &scriptedTerminal{input: strings.NewReader("help\r!echo shell\rexit\r")}
	if err := (Session{}).Interactive(context.Background(), rw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"routerd management CLI", "Configuration commands:", "unknown command; use help"} {
		if !strings.Contains(rw.output.String(), want) {
			t.Fatalf("missing terminal output %q", want)
		}
	}
}
