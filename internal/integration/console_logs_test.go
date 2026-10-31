//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/html"
)

// Read the same real log query through the private API and independent HTTP/SSH
// clients. Rows retain their original identity, including persisted old epochs.
func checkConsoleLogPage(t *testing.T, logs *localapi.Client, shell *ssh.Client, console func(string, string, int, string) string, path string) (logging.QueryResult, string, string) {
	t.Helper()
	request, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	query := request.Query()
	var result, remote logging.QueryResult
	command := "show logs"
	for _, field := range []string{"module", "level", "limit", "epoch", "after", "tail"} {
		if value := query.Get(field); value != "" {
			command += " " + field + " " + value
		}
	}
	var body string
	until(t, 5*time.Second, func() bool {
		if err := logs.Do(context.Background(), "GET", "/v1/logs?"+request.RawQuery, nil, &result); err != nil {
			t.Fatal(err)
		}
		body = console("GET", path, 200, "運用ログ")
		session, err := shell.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		output, err := session.CombinedOutput(command)
		_ = session.Close()
		if err != nil || json.Unmarshal(output, &remote) != nil {
			t.Fatalf("SSH log query: %v %s", err, output)
		}
		var after logging.QueryResult
		if err := logs.Do(context.Background(), "GET", "/v1/logs?"+request.RawQuery, nil, &after); err != nil {
			t.Fatal(err)
		}
		// A newly emitted product log can move the latest page between requests.
		// Compare only while its cursor is stable; never inject a synthetic record.
		return after.Cursor == result.Cursor
	})
	if !reflect.DeepEqual(remote.Records, result.Records) || remote.Cursor != result.Cursor ||
		remote.Reset != result.Reset || remote.Gap != result.Gap || remote.More != result.More || !slices.Equal(remote.GapReasons, result.GapReasons) {
		t.Fatalf("SSH changed original log records or continuation/gap metadata: SSH=%+v API=%+v", remote, result)
	}
	document, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var rows []logging.Cursor
	var rowText []string
	var next string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			attrs := make(map[string]string)
			for _, attr := range node.Attr {
				attrs[attr.Key] = attr.Val
			}
			if node.Data == "tr" && attrs["data-log-epoch"] != "" {
				sequence, err := strconv.ParseUint(attrs["data-log-sequence"], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				rows = append(rows, logging.Cursor{Epoch: attrs["data-log-epoch"], Sequence: sequence})
				var text strings.Builder
				var collect func(*html.Node)
				collect = func(n *html.Node) {
					if n.Type == html.TextNode {
						text.WriteString(n.Data)
					}
					for child := n.FirstChild; child != nil; child = child.NextSibling {
						collect(child)
					}
				}
				collect(node)
				rowText = append(rowText, text.String())
			}
			if node.Data == "a" && attrs["rel"] == "next" {
				if next != "" {
					t.Fatal("multiple continuation links")
				}
				next = attrs["href"]
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	if len(rows) != len(result.Records) {
		t.Fatalf("console log row count differs: HTTP=%d API=%d", len(rows), len(result.Records))
	}
	seen := make(map[logging.Cursor]bool)
	for i, entry := range result.Records {
		identity := logging.Cursor{Epoch: entry.Epoch, Sequence: entry.Sequence}
		if rows[i] != identity || seen[identity] || entry.Module != query.Get("module") || !slices.Contains([]string{"INFO", "WARN", "ERROR"}, entry.Level) {
			t.Fatalf("console log identity/filter mismatch: HTTP=%+v API=%+v", rows[i], entry)
		}
		seen[identity] = true
		for _, value := range []string{entry.Module, entry.Level, entry.Message} {
			if !strings.Contains(rowText[i], value) {
				t.Fatalf("console log row missing %q: %s", value, rowText[i])
			}
		}
	}
	continuation, err := url.Parse(next)
	if err != nil || continuation.Scheme != "" || continuation.Host != "" || continuation.Path != "/ops/logs" {
		t.Fatalf("invalid log continuation link %q: %v", next, err)
	}
	values := continuation.Query()
	for _, name := range []string{"module", "level", "limit"} {
		if values.Get(name) != query.Get(name) {
			t.Fatalf("log continuation lost %s filter: %s", name, next)
		}
	}
	if values.Get("epoch") != result.Cursor.Epoch || values.Get("after") != strconv.FormatUint(result.Cursor.Sequence, 10) || values.Get("tail") != "false" {
		t.Fatalf("console continuation differs from actual API cursor: %s %+v", next, result.Cursor)
	}
	return result, strings.TrimPrefix(continuation.RequestURI(), "/ops/"), body
}

func checkConsoleLogsBeforeRestart(t *testing.T, logs *localapi.Client, shell *ssh.Client, console func(string, string, int, string) string) logging.Cursor {
	t.Helper()
	const search = "logs?module=supervisor&level=INFO&limit=1"
	latest, _, _ := checkConsoleLogPage(t, logs, shell, console, search)
	if len(latest.Records) != 1 {
		t.Fatal("actual supervisor lifecycle log missing")
	}
	first, next, _ := checkConsoleLogPage(t, logs, shell, console, search+"&epoch="+latest.Cursor.Epoch+"&after=0")
	if len(first.Records) != 1 || !first.More {
		t.Fatal("actual supervisor logs do not provide a second page")
	}
	second, _, _ := checkConsoleLogPage(t, logs, shell, console, next)
	if len(second.Records) != 1 || second.Cursor.Epoch != first.Cursor.Epoch || second.Records[0].Sequence <= first.Records[0].Sequence {
		t.Fatalf("console log continuation repeated or skipped its epoch: first=%+v second=%+v", first.Cursor, second.Cursor)
	}
	t.Log("independent SSH and HTTP log search and continuation matched original supervisor records and API cursors; module/level/limit retained without duplicate rows")
	return second.Cursor
}

func checkConsoleLogsAfterRestart(t *testing.T, logs *localapi.Client, shell *ssh.Client, console func(string, string, int, string) string, old logging.Cursor) {
	t.Helper()
	path := fmt.Sprintf("logs?module=supervisor&level=INFO&limit=1&epoch=%s&after=%d", old.Epoch, old.Sequence)
	reset, next, body := checkConsoleLogPage(t, logs, shell, console, path)
	if !reset.Reset || !reset.Gap || !slices.Contains(reset.GapReasons, "logger_restart") || reset.Cursor.Epoch == old.Epoch {
		t.Fatalf("actual logger restart did not reset the old cursor: %+v", reset)
	}
	for _, message := range []string{"ログ担当が再起動しました", "ログに欠損がある可能性があります"} {
		if !strings.Contains(body, message) {
			t.Fatalf("console hid actual restart/gap: missing %q", message)
		}
	}
	continued, _, body := checkConsoleLogPage(t, logs, shell, console, next)
	if continued.Reset || continued.Cursor.Epoch != reset.Cursor.Epoch || strings.Contains(body, "ログ担当が再起動しました") {
		t.Fatalf("new cursor did not resume after logger restart: %+v", continued)
	}
	if len(reset.Records) != 1 || len(continued.Records) != 1 || continued.Records[0].Sequence <= reset.Records[0].Sequence {
		t.Fatal("new cursor repeated the recovered log page")
	}
	t.Log("actual logging SIGKILL/restart produced Reset/Gap in SSH and HTTP; following the new epoch cursor resumed original records")
}
