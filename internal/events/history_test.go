package events

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type archiveHook struct {
	bytes.Buffer
	once bool
	hook func()
}

func (w *archiveHook) Write(b []byte) (int, error) {
	if !w.once {
		w.once = true
		w.hook()
	}
	return w.Buffer.Write(b)
}

func TestHistoryExportFreezesTipWhileAppendContinuesAndDoesNotChangeConsumers(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Cursor(ctx, "slow"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err = s.Append(ctx, testEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	writer := &archiveHook{hook: func() {
		if _, err := s.Append(ctx, testEvent("later")); err != nil {
			t.Fatal(err)
		}
	}}
	result, err := s.ExportHistory(ctx, writer)
	if err != nil || result.Records != 2 || result.Header.TipCursor != 2 {
		t.Fatal("export did not freeze prefix", result, err)
	}
	verified, err := VerifyHistory(ctx, bytes.NewReader(writer.Bytes()))
	if err != nil || verified != result {
		t.Fatal("export cannot be independently verified", verified, err)
	}
	if cursor, err := s.Cursor(ctx, "slow"); err != nil || cursor != 0 {
		t.Fatal("export acknowledged another consumer", cursor, err)
	}
	if rows, err := s.List(ctx, 0, 16); err != nil || len(rows) != 3 {
		t.Fatal("export removed accepted history", rows, err)
	}
}

func TestHistoryVerificationRejectsTruncationModificationAndTrailingData(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Append(ctx, testEvent("original")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err = s.ExportHistory(ctx, &out); err != nil {
		t.Fatal(err)
	}
	good := out.String()
	for name, data := range map[string]string{
		"missing footer":        good[:strings.LastIndex(strings.TrimSuffix(good, "\n"), "\n")+1],
		"changed body":          strings.Replace(good, "original", "modified", 1),
		"changed cursor":        strings.Replace(good, `"cursor":1`, `"cursor":2`, 1),
		"trailing object":       good + "{}\n",
		"incomplete final line": strings.TrimSuffix(good, "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyHistory(ctx, strings.NewReader(data)); err == nil {
				t.Fatal("invalid archive passed")
			}
		})
	}
	if _, err = s.ExportHistory(ctx, failingArchiveWriter{}); err == nil {
		t.Fatal("failed writer reported durable export")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.ExportHistory(cancelled, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type failingArchiveWriter struct{}

func (failingArchiveWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
