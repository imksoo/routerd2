//go:build linux && integration

package release_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/protocol/webconsole"
	"github.com/imksoo/routerd2/internal/protocol/webconsoleconfig"
	"github.com/imksoo/routerd2/internal/release"
	"golang.org/x/net/html"
)

func startUpdateView(t *testing.T, ctx context.Context, runtimeDir string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	serving, stop := context.WithCancel(ctx)
	ready, stopped := make(chan struct{}), make(chan error, 1)
	go func() {
		stopped <- webconsole.Run(serving, webconsoleconfig.Config{Listen: netip.MustParseAddrPort(address), BasePath: "/ops/"}, runtimeDir, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-stopped:
		stop()
		t.Fatal(err)
	case <-ctx.Done():
		stop()
		<-stopped
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() {
		stop()
		if err := <-stopped; err != nil {
			t.Error(err)
		}
	})
	return "http://" + address + "/ops/updates"
}

// The existing fixture supplies states produced by the real signed store and
// control API. HTTP rendering does not establish a reboot or successful trial.
func checkUpdateView(t *testing.T, ctx context.Context, endpoint string, state release.State, wantState, knownGoodDigest string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("update view returned HTTP %d", response.StatusCode)
	}
	if state.Attempt != nil && strings.Contains(string(body), state.Attempt.Token) {
		t.Fatal("update view exposed the actual attempt token")
	}
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var text func(*html.Node) string
	text = func(node *html.Node) string {
		var value strings.Builder
		if node.Type == html.TextNode {
			value.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			value.WriteString(text(child))
		}
		return value.String()
	}
	fields, selections := map[string]string{}, map[string]string{}
	generation := ""
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		for _, attr := range node.Attr {
			switch attr.Key {
			case "data-update-generation":
				generation = attr.Val
			case "data-update-field":
				fields[attr.Val] = strings.TrimSpace(text(node))
			case "data-update-selection":
				selections[attr.Val] = text(node)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	if generation != strconv.FormatUint(state.Generation, 10) {
		t.Fatal("update view differs from the actual selection generation")
	}
	// /v1/version is served by this fixture's real control handler in-process.
	running := buildinfo.Current()
	if fields["running"] != fmt.Sprintf("%s（%s / %s）", running.ReleaseID, running.GOOS, running.GOARCH) {
		t.Fatalf("update view running version=%q; control executing version=%q", fields["running"], fmt.Sprintf("%s（%s / %s）", running.ReleaseID, running.GOOS, running.GOARCH))
	}
	expected := map[string]*release.Ref{"active": state.Active, "previous": state.Previous}
	if state.Pending != nil {
		expected["pending"] = &state.Pending.Release
	}
	count := 0
	for name, ref := range expected {
		if ref == nil {
			continue
		}
		count++
		row, ok := selections[name]
		if !ok || !strings.Contains(row, ref.ReleaseID) || !strings.Contains(row, ref.Digest) || !strings.Contains(row, strconv.FormatUint(ref.Sequence, 10)) {
			t.Fatalf("update view selection %s differs from the actual signed store", name)
		}
	}
	if len(selections) != count {
		t.Fatal("update view retains an absent selection")
	}
	if state.Pending != nil && state.Pending.Startup != nil && !strings.Contains(selections["pending"], state.Pending.Startup.Snapshot.Digest) {
		t.Fatal("update view differs from the reserved configuration snapshot")
	}
	if state.Attempt != nil {
		if fields["attempt"] != fmt.Sprintf("%d / %d", state.Attempt.Number, state.Pending.MaxAttempts) {
			t.Fatal("update view differs from the actual trial attempt")
		}
	} else if _, ok := fields["attempt"]; ok {
		t.Fatal("update view retains an absent attempt")
	}
	if fields["state"] != wantState {
		t.Fatalf("update view state=%q, want %q", fields["state"], wantState)
	}
	if knownGoodDigest != "" && !strings.Contains(fields["known-good"], knownGoodDigest) {
		t.Fatal("update view misidentifies the stored known-good version")
	}
	if state.LastFailure != nil {
		if fields["failure"] != state.LastFailure.Reason {
			t.Fatal("update view differs from the actual stored failure reason")
		}
		if !strings.Contains(string(body), "過去の記録です。現在の障害や復旧完了を示すものではありません。") {
			t.Fatal("update view presents a past failure as current recovery status")
		}
	} else if _, ok := fields["failure"]; ok {
		t.Fatal("update view retains an absent failure")
	}
	t.Logf("independent HTTP update view matched signed-store generation %d, %d selections and state %s; no reboot claimed", state.Generation, count, wantState)
}
