package events

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"sync"
)

// One bounded workspace per store. Only codec work is serialized; neither a
// SQLite connection nor the live publication/configuration lock is held here.
type archiveCodec struct {
	mu      sync.Mutex
	writer  *gzip.Writer
	reader  *gzip.Reader
	packed  bytes.Buffer
	decoded []byte
}

func (c *archiveCodec) encode(body []byte) ([]byte, error) {
	if len(body) < 1 || len(body) > maxBody {
		return nil, errors.New("invalid archive source size")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packed.Reset()
	if c.writer == nil {
		var err error
		c.writer, err = gzip.NewWriterLevel(&c.packed, gzip.BestSpeed)
		if err != nil {
			return nil, err
		}
	} else {
		c.writer.Reset(&c.packed)
	}
	_, err := c.writer.Write(body)
	if err = errors.Join(err, c.writer.Close()); err != nil {
		return nil, err
	}
	if c.packed.Len() > maxBody+1024 {
		return nil, errors.New("archive frame exceeds bound")
	}
	frame := bytes.NewReader(c.packed.Bytes())
	if c.reader == nil {
		c.reader, err = gzip.NewReader(frame)
	} else {
		err = c.reader.Reset(frame)
	}
	if err != nil {
		return nil, err
	}
	c.reader.Multistream(false)
	if len(c.decoded) < len(body)+1 {
		c.decoded = make([]byte, len(body)+1)
	}
	// The extra byte forces EOF/CRC verification and detects excess expansion.
	n, readErr := io.ReadFull(c.reader, c.decoded[:len(body)+1])
	closeErr := c.reader.Close()
	if readErr != io.ErrUnexpectedEOF || n != len(body) || closeErr != nil || frame.Len() != 0 || !bytes.Equal(body, c.decoded[:n]) {
		return nil, errors.Join(errors.New("event archive round trip failed"), closeErr)
	}
	return bytes.Clone(c.packed.Bytes()), nil
}

func (c *archiveCodec) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writer, c.reader, c.decoded = nil, nil, nil
	c.packed = bytes.Buffer{}
}
