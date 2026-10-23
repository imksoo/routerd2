// Package worker implements the private lifecycle channel used by routerd workers.
package worker

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const (
	ProtocolVersion = 1
	MaxFrameSize    = 64 << 10
	ControlFDEnv    = "ROUTERD_CONTROL_FD"
	ModuleNameEnv   = "ROUTERD_MODULE_NAME"
)

// Message is a lifecycle message, not a forwarding or configuration API.
// Instance identity is established by the private inherited descriptor.
type Message struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`
	Role      string `json:"role,omitempty"`
	ReleaseID string `json:"release_id,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Sequence  uint64 `json:"sequence,omitempty"`
	Error     string `json:"error,omitempty"`
}

// WriteMessage sends one bounded, length-prefixed JSON message. Its caller must
// serialize concurrent writes and arrange deadlines on network connections.
func WriteMessage(w io.Writer, m Message) error {
	if m.Version == 0 {
		m.Version = ProtocolVersion
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode lifecycle message: %w", err)
	}
	if len(b) == 0 || len(b) > MaxFrameSize {
		return fmt.Errorf("lifecycle frame size %d exceeds limit", len(b))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(b)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, b)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func ReadMessage(r io.Reader) (Message, error) {
	var m Message
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return m, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrameSize {
		return m, fmt.Errorf("invalid lifecycle frame size %d", size)
	}
	b := make([]byte, int(size))
	if _, err := io.ReadFull(r, b); err != nil {
		return m, fmt.Errorf("read lifecycle frame: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("decode lifecycle frame: %w", err)
	}
	if m.Version != ProtocolVersion {
		return m, fmt.Errorf("unsupported lifecycle version %d", m.Version)
	}
	if m.Type == "" {
		return m, fmt.Errorf("lifecycle message has no type")
	}
	return m, nil
}
