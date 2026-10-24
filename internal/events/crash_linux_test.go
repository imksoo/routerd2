//go:build linux

package events

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv("ROUTERD2_EVENTS_CRASH_HELPER"); dir != "" {
		ctx := context.Background()
		s, err := Open(Options{Directory: dir})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err = s.Cursor(ctx, "consumer"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		r, err := s.Append(ctx, testEvent("committed-before-kill"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err = s.Schedule(ctx, "consumer", 0, r.Cursor, []Job{{Key: "committed-work", Payload: json.RawMessage(`{}`)}}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		checkpoint, err := s.Checkpoint(ctx, "stateful")
		if err == nil {
			err = s.Transition(ctx, "stateful", checkpoint, checkpoint.Cursor, json.RawMessage(`{"count":1}`), []Job{{Key: "stateful-work", Payload: json.RawMessage(`{}`)}})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		producer, err := s.ProducerCheckpoint(ctx, "derived")
		if err == nil {
			err = s.ProducerTransition(ctx, "derived", producer, json.RawMessage(`{"asserted":true}`), []Job{{Key: "producer-work", Payload: json.RawMessage(`{}`)}})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, "EVENTS_COMMIT_SYNCED")
		var wait [1]byte
		_, _ = os.Stdin.Read(wait[:]) // parent SIGKILLs without SQLite/Store.Close
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func TestCommittedJournalCursorAndWorkSurviveSIGKILL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "events")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "ROUTERD2_EVENTS_CRASH_HELPER="+dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		ready <- scanner.Scan() && scanner.Text() == "EVENTS_COMMIT_SYNCED"
	}()
	select {
	case ok := <-ready:
		if !ok {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			waited = true
			t.Fatal("child failed before durable commit", stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("child durable commit timed out")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err == nil {
		t.Fatal("helper did not exit abruptly")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("helper did not exit by SIGKILL", err)
	}
	s, err := Open(Options{Directory: dir})
	if err != nil {
		t.Fatal("WAL recovery after SIGKILL", err)
	}
	defer s.Close()
	rows, err := s.List(context.Background(), 0, 256)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	cursor, err := s.Cursor(context.Background(), "consumer")
	if err != nil || cursor != rows[0].Cursor {
		t.Fatal("cursor lost after committed work", cursor, err)
	}
	work, err := s.Pending(context.Background(), "consumer", 256)
	if err != nil || len(work) != 1 || work[0].EventCursor != cursor {
		t.Fatal("work lost after committed cursor", work, err)
	}
	state, err := s.Checkpoint(context.Background(), "stateful")
	if err != nil || state.Cursor != cursor || state.Version != 1 || string(state.State) != `{"count":1}` {
		t.Fatal("state lost across SIGKILL", state, err)
	}
	work, err = s.Pending(context.Background(), "stateful", 10)
	if err != nil || len(work) != 1 || work[0].Key != "stateful-work" {
		t.Fatal("stateful work lost across SIGKILL", work, err)
	}
	producer, err := s.ProducerCheckpoint(context.Background(), "derived")
	if err != nil || producer.Version != 1 || string(producer.State) != `{"asserted":true}` {
		t.Fatal("producer state lost across SIGKILL", producer, err)
	}
	producerWork, err := s.ProducerPending(context.Background(), "derived", 10)
	if err != nil || len(producerWork) != 1 || producerWork[0].Key != "producer-work" || producerWork[0].StateVersion != 1 {
		t.Fatal("producer work lost across SIGKILL", producerWork, err)
	}
}

func TestForeignDatabaseIsRejectedWithoutChangingItsBytes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "events")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "journal.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE foreign_state(value TEXT); INSERT INTO foreign_state VALUES('preserve'); PRAGMA application_id=7;`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if store, err := Open(Options{Directory: dir}); err == nil {
		store.Close()
		t.Fatal("foreign database accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("foreign database changed during rejected open", err)
	}
}
