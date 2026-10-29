package events

import (
	"bytes"
	"compress/gzip"
	"errors"
	"sync"
)

// One bounded workspace per store. Only codec work is serialized; neither a
// SQLite connection nor the live publication/configuration lock is held here.
type archiveCodec struct {
	mu     sync.Mutex
	writer *gzip.Writer
	packed bytes.Buffer
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
	return bytes.Clone(c.packed.Bytes()), nil
}

func (c *archiveCodec) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writer = nil
	c.packed = bytes.Buffer{}
}
