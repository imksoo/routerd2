// Package config owns the durable desired state. The kernel is a projection of it.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

const SchemaVersion = 1
const MaxSize = 2 << 20

type Document struct {
	SchemaVersion int                     `json:"schema_version"`
	Network       network.Desired         `json:"network"`
	Services      []serviceconfig.Service `json:"services,omitempty"`
}

type Committed struct {
	Revision        uint64        `json:"revision"`
	Config          Document      `json:"config"`
	LastRequestID   string        `json:"last_request_id,omitempty"`
	LastRequestHash string        `json:"last_request_hash,omitempty"`
	Confirmation    *Confirmation `json:"confirmation,omitempty"`
}

// Confirmation is persisted with the candidate revision before it is acknowledged.
// A control-worker restart rolls an unconfirmed revision back immediately.
type Confirmation struct {
	Previous         Document  `json:"previous"`
	PreviousRevision uint64    `json:"previous_revision"`
	Deadline         time.Time `json:"deadline"`
	RequestID        string    `json:"request_id"`
}

func Validate(d Document) error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported config schema %d (expected %d)", d.SchemaVersion, SchemaVersion)
	}
	if err := network.Validate(d.Network); err != nil {
		return err
	}
	if err := serviceconfig.Validate(d.Services); err != nil {
		return err
	}
	for _, service := range d.Services {
		if service.RAClient == nil {
			continue
		}
		for _, link := range d.Network.Interfaces {
			if link.Name == service.Interface() && (link.IPv6AcceptRA != nil && *link.IPv6AcceptRA != 0 || link.IPv6Autoconf != nil && *link.IPv6Autoconf) {
				return fmt.Errorf("RA client %s requires exclusive RA/SLAAC ownership; disable kernel ipv6_accept_ra and ipv6_autoconf", service.Name)
			}
		}
	}
	// Dynamically created links have exactly one owner. Firewall interface
	// matches may refer to them before creation; static links/routes/services
	// must not keep them alive while their protocol dependency is unavailable.
	reserved := map[string]string{}
	for _, service := range d.Services {
		name := ""
		if service.VRRP != nil {
			name = service.VRRPConfig().VIPInterface
		}
		if service.DSLite != nil {
			name = service.DSLite.Tunnel
			if service.DSLite.LocalAddress != "" {
				found := false
				for _, iface := range d.Network.Interfaces {
					if iface.Name != service.Interface() {
						continue
					}
					for _, address := range iface.Addresses {
						prefix, err := netip.ParsePrefix(address)
						if err == nil && prefix.Addr().String() == service.DSLite.LocalAddress {
							found = true
						}
					}
				}
				if !found {
					return fmt.Errorf("DS-Lite %s local_address must be committed on its underlay interface", service.Name)
				}
			}
		}
		if name == "" {
			continue
		}
		if owner := reserved[name]; owner != "" {
			return fmt.Errorf("services %s and %s share dynamic interface %s", owner, service.Name, name)
		}
		reserved[name] = service.Name
	}
	for _, link := range d.Network.Links {
		if owner := reserved[link.Name]; owner != "" {
			return fmt.Errorf("static link %s conflicts with dynamic service %s", link.Name, owner)
		}
	}
	for _, link := range d.Network.Interfaces {
		if owner := reserved[link.Name]; owner != "" {
			return fmt.Errorf("static interface %s conflicts with dynamic service %s", link.Name, owner)
		}
	}
	for _, link := range d.Network.DSLite {
		if owner := reserved[link.Name]; owner != "" {
			return fmt.Errorf("static DS-Lite %s conflicts with dynamic service %s", link.Name, owner)
		}
	}
	for _, route := range d.Network.Routes {
		if owner := reserved[route.Interface]; owner != "" {
			return fmt.Errorf("static route cannot use dynamic interface owned by %s", owner)
		}
		for _, hop := range route.NextHops {
			if owner := reserved[hop.Interface]; owner != "" {
				return fmt.Errorf("static route cannot use dynamic interface owned by %s", owner)
			}
		}
	}
	for _, service := range d.Services {
		if owner := reserved[service.Interface()]; owner != "" {
			return fmt.Errorf("service %s cannot use dynamic interface of %s without a dependency", service.Name, owner)
		}
	}
	return nil
}

func Decode(r io.Reader, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return err
	}
	if len(b) > MaxSize {
		return errors.New("configuration exceeds 2 MiB")
	}
	return decodeYAML(b, v)
}

func Read(path string) (Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return Document{}, err
	}
	defer f.Close()
	var d Document
	if err := Decode(f, &d); err != nil {
		return d, err
	}
	return d, Validate(d)
}

type Store struct{ Dir string }

func Open(dir, bootstrap string) (*Store, Committed, error) {
	s := &Store{Dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, Committed{}, err
	}
	c, err := s.Load()
	if err == nil {
		return s, c, nil
	}
	if !os.IsNotExist(err) {
		return nil, c, err
	}
	d, err := Read(bootstrap)
	if err != nil {
		return nil, c, fmt.Errorf("bootstrap config: %w", err)
	}
	c = Committed{Revision: 1, Config: d}
	if err = s.Save(c); err != nil {
		return nil, c, err
	}
	return s, c, nil
}

func (s *Store) Load() (Committed, error) {
	var c Committed
	f, err := os.Open(filepath.Join(s.Dir, "config.json"))
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err = Decode(f, &c); err != nil {
		return c, err
	}
	if c.Revision == 0 {
		return c, errors.New("invalid committed revision zero")
	}
	if c.Confirmation != nil {
		if c.Confirmation.PreviousRevision == 0 || c.Confirmation.PreviousRevision >= c.Revision || c.Confirmation.RequestID != c.LastRequestID || c.Confirmation.Deadline.IsZero() {
			return c, errors.New("invalid confirmation recovery record")
		}
		if err := Validate(c.Confirmation.Previous); err != nil {
			return c, fmt.Errorf("invalid rollback configuration: %w", err)
		}
	}
	return c, Validate(c.Config)
}

func (s *Store) Save(c Committed) error  { return AtomicJSON(filepath.Join(s.Dir, "config.json"), c) }
func (s *Store) Stage(c Committed) error { return AtomicJSON(filepath.Join(s.Dir, "pending.json"), c) }
func (s *Store) ClearPending() error {
	err := os.Remove(filepath.Join(s.Dir, "pending.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(s.Dir)
}

// AtomicJSON fsyncs the new file and its parent directory. Existing files are
// never opened for in-place modification, so a reader sees a complete revision.
func AtomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".routerd-txn-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
