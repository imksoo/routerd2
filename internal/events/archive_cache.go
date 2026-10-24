package events

import (
	"bytes"
	"container/list"
	"sync"
)

const archiveCacheBytes = 4 << 20
const archiveCacheRows = 4096

type archivedRead struct {
	cursor               uint64
	size                 int64
	packed, digest, body []byte
}

func (r archivedRead) weight() int { return len(r.packed) + len(r.digest) + len(r.body) }

type archivedReadCache struct {
	mu    sync.Mutex
	rows  map[uint64]*list.Element
	order list.List
	bytes int
}

func (c *archivedReadCache) get(cursor uint64, packed, digest []byte, size int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.rows[cursor]
	if entry == nil {
		return nil, false
	}
	r := entry.Value.(archivedRead)
	// SQL bytes and metadata must still match. A corrupted stored frame must
	// not be hidden by a previously verified decoded copy.
	if r.size != size || !bytes.Equal(r.packed, packed) || !bytes.Equal(r.digest, digest) {
		return nil, false
	}
	c.order.MoveToFront(entry)
	return r.body, true
}

func (c *archivedReadCache) put(r archivedRead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = map[uint64]*list.Element{}
	}
	if old := c.rows[r.cursor]; old != nil {
		c.bytes -= old.Value.(archivedRead).weight()
		c.order.Remove(old)
		delete(c.rows, r.cursor)
	}
	c.rows[r.cursor] = c.order.PushFront(r)
	c.bytes += r.weight()
	for c.bytes > archiveCacheBytes || len(c.rows) > archiveCacheRows {
		old := c.order.Back()
		c.bytes -= old.Value.(archivedRead).weight()
		delete(c.rows, old.Value.(archivedRead).cursor)
		c.order.Remove(old)
	}
}

func (c *archivedReadCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = nil
	c.order.Init()
	c.bytes = 0
}

func (s *Store) loadBody(cursor uint64, hot, packed []byte, size int64, digest []byte) ([]byte, error) {
	if len(hot) > 0 {
		return storedBody(hot, packed, size, digest)
	}
	if body, ok := s.archiveCache.get(cursor, packed, digest, size); ok {
		return body, nil
	}
	body, err := archivedBody(packed, size, digest)
	if err == nil {
		s.archiveCache.put(archivedRead{cursor: cursor, size: size, packed: packed, digest: digest, body: body})
	}
	return body, err
}
