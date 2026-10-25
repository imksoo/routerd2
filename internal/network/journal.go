package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/imksoo/routerd2/internal/strictjson"
)

const MaxJournalBytes = 64 << 20
const maxJournalNodes = 2_000_000

var ErrJournalBusy = errors.New("network journal writer is busy")

// JournalInspection describes one verified persisted record, not a kernel
// observation or a cross-worker snapshot. Present=false means a checked parent
// contained no journal. An inaccessible/missing parent is an error instead.
// No ownership bodies, secret paths, secret hashes or lease facts are returned.
type JournalInspection struct {
	Present           bool   `json:"present"`
	Version           int    `json:"version"`
	Revision          uint64 `json:"revision"`
	AttemptedRevision uint64 `json:"attempted_revision"`
	Status            string `json:"status"`
	SHA256            string `json:"sha256"`
}

func InspectJournal(ctx context.Context, path string) (JournalInspection, error) {
	if err := ctx.Err(); err != nil {
		return JournalInspection{}, err
	}
	dir, name, err := openNetworkJournalParent(path, false)
	if err != nil {
		return JournalInspection{}, err
	}
	defer dir.Close()
	_, inspection, err := readNetworkJournal(ctx, dir, name)
	return inspection, err
}

// InspectJournal reads the actual pinned journal while excluding this writer.
// It never returns cached in-memory state in place of an unreadable file.
func (e *Engine) InspectJournal(ctx context.Context) (JournalInspection, error) {
	if err := ctx.Err(); err != nil {
		return JournalInspection{}, err
	}
	if !e.mu.TryLock() {
		return JournalInspection{}, ErrJournalBusy
	}
	defer e.mu.Unlock()
	if e.lock == nil {
		return JournalInspection{}, errors.New("network engine is closed")
	}
	_, inspection, err := readNetworkJournal(ctx, e.directory, e.journalName)
	if err == nil && e.journalPresent && !inspection.Present {
		err = fmt.Errorf("network journal disappeared: %w", os.ErrNotExist)
	}
	return inspection, err
}

func readNetworkJournal(ctx context.Context, dir *os.File, name string) (journal, JournalInspection, error) {
	f, err := openNetworkJournalFile(dir, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return journal{}, JournalInspection{}, nil
	}
	if err != nil {
		return journal{}, JournalInspection{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return journal{}, JournalInspection{}, err
	}
	if info.Size() <= 0 || info.Size() > MaxJournalBytes {
		return journal{}, JournalInspection{}, errors.New("network journal exceeds its nonempty 64 MiB bound")
	}
	raw, err := io.ReadAll(io.LimitReader(journalContextReader{ctx: ctx, r: f}, MaxJournalBytes+1))
	if err != nil {
		return journal{}, JournalInspection{}, err
	}
	if len(raw) > MaxJournalBytes {
		return journal{}, JournalInspection{}, errors.New("network journal exceeds its byte bound")
	}
	var state journal
	if err := strictjson.Decode(raw, &state, maxJournalNodes); err != nil {
		return journal{}, JournalInspection{}, fmt.Errorf("network ownership journal: %w", err)
	}
	if state.Version != JournalVersion {
		return journal{}, JournalInspection{}, fmt.Errorf("unsupported network journal version %d", state.Version)
	}
	switch state.Status {
	case "unapplied", "applying", "applied", "degraded":
	default:
		return journal{}, JournalInspection{}, errors.New("invalid network journal status")
	}
	if err := validateJournal(state); err != nil {
		return journal{}, JournalInspection{}, fmt.Errorf("invalid network ownership journal: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return journal{}, JournalInspection{}, err
	}
	sum := sha256.Sum256(raw)
	return state, JournalInspection{Present: true, Version: state.Version, Revision: state.Revision, AttemptedRevision: state.AttemptedRevision, Status: state.Status, SHA256: hex.EncodeToString(sum[:])}, nil
}

type journalContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r journalContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func encodeNetworkJournal(state journal) ([]byte, error) {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxJournalBytes {
		return nil, errors.New("network journal exceeds its byte bound")
	}
	// Never publish a body our reader would refuse for structural complexity.
	if err := strictjson.Check(raw, maxJournalNodes); err != nil {
		return nil, err
	}
	return raw, nil
}
