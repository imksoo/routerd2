package events

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
)

// History archives are immutable event-prefix copies, not database backups.
// They contain no consumer/producer checkpoint and never acknowledge work.
type ArchiveHeader struct {
	Kind        string `json:"kind"`
	Format      string `json:"format"`
	Version     int    `json:"version"`
	FirstCursor uint64 `json:"first_cursor"`
	TipCursor   uint64 `json:"tip_cursor"`
	Records     int64  `json:"records"`
	EventBytes  int64  `json:"event_bytes"`
}

type ArchiveSummary struct {
	Header     ArchiveHeader `json:"header"`
	Records    int64         `json:"records"`
	EventBytes int64         `json:"event_bytes"`
	SHA256     string        `json:"sha256"`
}

type archiveRecord struct {
	Kind   string `json:"kind"`
	Cursor uint64 `json:"cursor"`
	Event  Event  `json:"event"`
}
type archiveFooter struct {
	Kind       string `json:"kind"`
	Records    int64  `json:"records"`
	EventBytes int64  `json:"event_bytes"`
	SHA256     string `json:"sha256"`
}

func (c Capacity) HistoryHeader() ArchiveHeader {
	return ArchiveHeader{Kind: "header", Format: "routerd-event-history", Version: 1, FirstCursor: c.FirstCursor, TipCursor: c.TipCursor, Records: c.History.Rows, EventBytes: c.History.Bytes}
}

func validHeader(h ArchiveHeader) bool {
	if h.Kind != "header" || h.Format != "routerd-event-history" || h.Version != 1 || h.Records < 0 || h.Records > 1000000 || h.EventBytes < 0 || h.EventBytes > 1<<30 || h.TipCursor > math.MaxInt64 {
		return false
	}
	if h.Records == 0 {
		return h.FirstCursor == 0 && h.TipCursor == 0 && h.EventBytes == 0
	}
	return h.FirstCursor > 0 && h.TipCursor >= h.FirstCursor && uint64(h.Records) <= h.TipCursor-h.FirstCursor+1 && h.EventBytes > 0
}

func writeArchiveLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func (s *Store) ExportHistory(ctx context.Context, w io.Writer) (ArchiveSummary, error) {
	c, err := s.Capacity(ctx)
	if err != nil {
		return ArchiveSummary{}, err
	}
	return ExportHistory(ctx, w, c.HistoryHeader(), s.List)
}

// ExportHistory freezes the supplied tip, then fetches bounded pages without
// holding a transaction, store lock or SQLite read snapshot across writer IO.
// Append may continue. A missing/changed prefix fails instead of skipping data.
func ExportHistory(ctx context.Context, w io.Writer, h ArchiveHeader, fetch func(context.Context, uint64, int) ([]Record, error)) (ArchiveSummary, error) {
	if !validHeader(h) || w == nil || fetch == nil {
		return ArchiveSummary{}, errors.New("invalid bounded history export")
	}
	if err := ctx.Err(); err != nil {
		return ArchiveSummary{}, err
	}
	digest := sha256.New()
	hashed := io.MultiWriter(w, digest)
	if err := writeArchiveLine(hashed, h); err != nil {
		return ArchiveSummary{}, err
	}
	result := ArchiveSummary{Header: h}
	after := uint64(0)
	ids := map[string]bool{}
	for after < h.TipCursor {
		if err := ctx.Err(); err != nil {
			return ArchiveSummary{}, err
		}
		rows, err := fetch(ctx, after, 16)
		if err != nil {
			return ArchiveSummary{}, err
		}
		if len(rows) == 0 || len(rows) > 16 {
			return ArchiveSummary{}, errors.New("missing or oversized history page")
		}
		before := after
		for _, record := range rows {
			if record.Cursor > h.TipCursor {
				break
			}
			body, err := encodeEvent(record.Event)
			if err != nil {
				return ArchiveSummary{}, err
			}
			if record.Cursor <= after || ids[record.Event.ID] || result.Records == 0 && record.Cursor != h.FirstCursor {
				return ArchiveSummary{}, errors.New("history identity or cursor conflict")
			}
			result.Records++
			result.EventBytes += int64(len(body))
			if result.Records > h.Records || result.EventBytes > h.EventBytes {
				return ArchiveSummary{}, errors.New("history prefix exceeds snapshot")
			}
			if err = writeArchiveLine(hashed, archiveRecord{Kind: "event", Cursor: record.Cursor, Event: record.Event}); err != nil {
				return ArchiveSummary{}, err
			}
			ids[record.Event.ID] = true
			after = record.Cursor
		}
		if after == before {
			return ArchiveSummary{}, errors.New("history page made no progress")
		}
	}
	if result.Records != h.Records || result.EventBytes != h.EventBytes {
		return ArchiveSummary{}, errors.New("history prefix does not match snapshot")
	}
	result.SHA256 = hex.EncodeToString(digest.Sum(nil))
	if err := ctx.Err(); err != nil {
		return ArchiveSummary{}, err
	}
	err := writeArchiveLine(w, archiveFooter{Kind: "footer", Records: result.Records, EventBytes: result.EventBytes, SHA256: result.SHA256})
	return result, err
}

func decodeArchiveLine(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing archive JSON")
	}
	return nil
}

// VerifyHistory checks framing, complete event bodies, unique IDs, increasing
// cursors, counts/bytes and the header+records SHA. SHA is integrity, not origin
// authentication. This operation neither imports nor reactivates any state.
func VerifyHistory(ctx context.Context, r io.Reader) (ArchiveSummary, error) {
	reader := bufio.NewReaderSize(r, maxBody+2048)
	line := func() ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := reader.ReadSlice('\n')
		if err != nil {
			return nil, errors.Join(errors.New("incomplete or oversized archive line"), err)
		}
		return b, nil
	}
	first, err := line()
	if err != nil {
		return ArchiveSummary{}, err
	}
	var h ArchiveHeader
	if err = decodeArchiveLine(first, &h); err != nil || !validHeader(h) {
		return ArchiveSummary{}, errors.Join(errors.New("invalid archive header"), err)
	}
	digest := sha256.New()
	_, _ = digest.Write(first)
	result := ArchiveSummary{Header: h}
	ids := map[string]bool{}
	after := uint64(0)
	for result.Records < h.Records {
		b, err := line()
		if err != nil {
			return ArchiveSummary{}, err
		}
		var record archiveRecord
		if err = decodeArchiveLine(b, &record); err != nil {
			return ArchiveSummary{}, err
		}
		body, err := encodeEvent(record.Event)
		if err != nil {
			return ArchiveSummary{}, err
		}
		if record.Kind != "event" || record.Cursor <= after || record.Cursor > h.TipCursor || ids[record.Event.ID] || result.Records == 0 && record.Cursor != h.FirstCursor {
			return ArchiveSummary{}, errors.New("archive record identity/cursor conflict")
		}
		ids[record.Event.ID] = true
		after = record.Cursor
		result.Records++
		result.EventBytes += int64(len(body))
		if result.EventBytes > h.EventBytes {
			return ArchiveSummary{}, errors.New("archive body bytes exceed snapshot")
		}
		_, _ = digest.Write(b)
	}
	if after != h.TipCursor || result.EventBytes != h.EventBytes {
		return ArchiveSummary{}, errors.New("archive prefix does not match header")
	}
	last, err := line()
	if err != nil {
		return ArchiveSummary{}, err
	}
	var f archiveFooter
	if err = decodeArchiveLine(last, &f); err != nil {
		return ArchiveSummary{}, err
	}
	result.SHA256 = hex.EncodeToString(digest.Sum(nil))
	if f.Kind != "footer" || f.Records != result.Records || f.EventBytes != result.EventBytes || f.SHA256 != result.SHA256 {
		return ArchiveSummary{}, errors.New("archive footer integrity mismatch")
	}
	if b, err := reader.ReadSlice('\n'); len(b) != 0 || err != io.EOF {
		return ArchiveSummary{}, errors.New("trailing archive data")
	}
	return result, nil
}
