// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	_ "embed"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
)

//go:embed logs.html
var logsHTML string

type logPage struct {
	Module string
	Level  string
	Limit  string
	Result *logging.QueryResult
	Next   string
}

func loadLogs(ctx context.Context, client *localapi.Client, query url.Values, basePath string) (*logPage, error) {
	p := &logPage{Module: query.Get("module"), Level: strings.ToUpper(query.Get("level")), Limit: query.Get("limit")}
	if p.Limit == "" {
		p.Limit = "100"
	}
	var result logging.QueryResult
	// Keep validation and cursor semantics with the log owner, including
	// duplicate or unknown parameters and the initial tail selection.
	if err := client.Do(ctx, http.MethodGet, "/v1/logs?"+query.Encode(), nil, &result); err != nil {
		return p, err
	}
	p.Result = &result
	next := url.Values{"epoch": {result.Cursor.Epoch}, "after": {strconv.FormatUint(result.Cursor.Sequence, 10)}, "tail": {"false"}, "limit": {p.Limit}}
	if p.Module != "" {
		next.Set("module", p.Module)
	}
	if p.Level != "" {
		next.Set("level", p.Level)
	}
	p.Next = basePath + "logs?" + next.Encode()
	return p, nil
}

func logGapReason(reason string) string {
	switch reason {
	case "logger_restart":
		return "ログ担当が再起動しました"
	case "cursor_ahead":
		return "指定した継続位置が現在の末尾より先です"
	case "memory_retention":
		return "メモリーの保持範囲外にログがあります"
	case "persistence_gap":
		return "保存から読み戻したログに連番の欠損があります"
	case "producer_drop":
		return "発生元でログの欠損が観測されました"
	default:
		return "未分類の欠損です"
	}
}
