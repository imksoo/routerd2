// Package config owns the durable desired state. The kernel is a projection of it.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

const SchemaVersion = resourceprogram.SchemaVersion

// JournalVersion names the implicit store identity/adoption/outbox codec. It
// does not add a version header or a reader for discarded prototype records.
const JournalVersion = 1
const MaxSize = 2 << 20

// Leave room for revision, request ID, confirmation controls and JSON framing
// when an individual document is carried by a bounded module API request.
const MaxDocumentSize = MaxSize - (4 << 10)

// A durable record can contain both the candidate and the previous document
// during confirmed commit. Its limit is separate from the public input limit.
// Read and write use the same finite budget; prototype journals are not an
// input format of the ResourceProgram store.
const MaxRecordSize = 2*MaxSize + (64 << 10)
const MaxStoredReadSize = MaxRecordSize

// Document is the operator's source program. Only this source is persisted and
// edited. Native is a derived in-memory input for the existing kernel/protocol
// engines and is never accepted as an alternative configuration format.
type Document = resourceprogram.Program
type Native = resourceprogram.Native

// FromNative constructs a source program for internal importers and fixtures.
// It does not decode the discarded routerd2 prototype configuration format.
func FromNative(native Native) Document { return resourceprogram.FromNative(native) }

type Committed struct {
	Startup         *configref.Adoption `json:"startup,omitempty"`
	StoreID         string              `json:"store_id"`
	Revision        uint64              `json:"revision"`
	Config          Document            `json:"config"`
	LastRequestID   string              `json:"last_request_id,omitempty"`
	LastRequestHash string              `json:"last_request_hash,omitempty"`
	Confirmation    *Confirmation       `json:"confirmation,omitempty"`
	EventOutbox     []ChangeEvent       `json:"event_outbox,omitempty"`
}

// ChangeEvent is persisted atomically with the authoritative configuration.
// It contains no configuration bodies, credentials or caller-controlled text.
// Delivery to the observation journal may retry after a process crash.
type ChangeEvent struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Revision   uint64    `json:"revision"`
	ObservedAt time.Time `json:"observed_at"`
}

const MaxEventOutbox = 64

// Confirmation is persisted with the candidate revision before it is acknowledged.
// A control-worker restart rolls an unconfirmed revision back immediately.
type Confirmation struct {
	Previous         Document  `json:"previous"`
	PreviousRevision uint64    `json:"previous_revision"`
	Deadline         time.Time `json:"deadline"`
	RequestID        string    `json:"request_id"`
}

func Validate(d Document) error {
	_, err := Compile(d)
	return err
}

// Compile is pure. Source validation and cross-worker ownership checks complete
// before a caller changes the journal, kernel or supervised process set.
func Compile(d Document) (resourceprogram.Plan, error) {
	canonical, err := json.Marshal(d)
	if err != nil {
		return resourceprogram.Plan{}, fmt.Errorf("encode configuration: %w", err)
	}
	if len(canonical) > MaxDocumentSize {
		return resourceprogram.Plan{}, errors.New("canonical JSON configuration exceeds its 2 MiB budget including the 4 KiB protocol envelope reserve")
	}
	if err := checkJSONShape(canonical, maxDocumentNodes); err != nil {
		return resourceprogram.Plan{}, err
	}
	plan, err := resourceprogram.Compile(d)
	if err != nil {
		return resourceprogram.Plan{}, err
	}
	if err := validateNative(plan.Native, d); err != nil {
		return resourceprogram.Plan{}, err
	}
	return plan, nil
}

func validateNative(d Native, source Document) error {
	for _, service := range d.Services {
		if service.VRRP != nil && service.VRRP.GracefulActivation != nil && conditionRequiresFact(&service.VRRP.GracefulActivation.ReadyWhen, service.ModuleName()+".active") {
			return fmt.Errorf("VRRP %s publication cannot require its own unpublished active fact", service.Name)
		}
	}
	if err := validateBFDRouting(d); err != nil {
		return err
	}
	domains := network.RoutingDomains(d.Network)
	for _, service := range d.Services {
		if relay := service.DHCPRelay; relay != nil {
			if relay.VRF != "" && domains[relay.VRF].VRF != relay.VRF {
				return fmt.Errorf("DHCP relay %s requires a declared VRF", service.Name)
			}
			for _, client := range relay.Interfaces {
				owned := false
				for _, iface := range d.Network.Interfaces {
					if iface.Name != client.Name || domains[iface.Name].VRF != relay.VRF {
						continue
					}
					for _, raw := range iface.Addresses {
						prefix, err := netip.ParsePrefix(raw)
						owned = owned || err == nil && prefix.Addr().String() == client.Address
					}
				}
				if !owned {
					return fmt.Errorf("DHCP relay %s requires giaddr committed on interface %s in its routing domain", service.Name, client.Name)
				}
			}
		}
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
		if service.PPPoE != nil {
			name = service.PPPoE.PPPInterface
		}
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
		if owner := reserved[service.Interface()]; owner != "" && !guardedDynamicDependency(source, service.Name, owner) {
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

type Store struct {
	Dir        string
	checkpoint func(string) error // test-only publication interruptions; nil in production
}

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
	var identity [16]byte
	if _, err = io.ReadFull(rand.Reader, identity[:]); err != nil {
		return nil, c, err
	}
	c = Committed{StoreID: hex.EncodeToString(identity[:]), Revision: 1, Config: d}
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
	data, err := io.ReadAll(io.LimitReader(f, MaxStoredReadSize+1))
	if err != nil {
		return c, err
	}
	if len(data) > MaxStoredReadSize {
		return c, fmt.Errorf("stored configuration record exceeds %d bytes", MaxStoredReadSize)
	}
	if err := decodeRecord(data, &c); err != nil {
		return c, err
	}
	return c, validateRecord(c)
}

func validateRecord(c Committed) error {
	if c.Startup != nil {
		if err := c.Startup.Validate(c.StoreID, c.Revision); err != nil {
			return err
		}
	}
	if !validStoreID(c.StoreID) {
		return errors.New("committed configuration requires a lower-case 128-bit store identity")
	}
	if c.Revision == 0 {
		return errors.New("invalid committed revision zero")
	}
	if len(c.LastRequestID) > 128 || len(c.LastRequestHash) > 128 {
		return errors.New("committed request metadata exceeds its bound")
	}
	if err := validateEventOutbox(c); err != nil {
		return err
	}
	if c.Confirmation != nil {
		if c.Confirmation.PreviousRevision == 0 || c.Confirmation.PreviousRevision >= c.Revision || c.Confirmation.RequestID == "" || c.Confirmation.RequestID != c.LastRequestID || c.Confirmation.Deadline.IsZero() {
			return errors.New("invalid confirmation recovery record")
		}
		if err := Validate(c.Confirmation.Previous); err != nil {
			return fmt.Errorf("invalid rollback configuration: %w", err)
		}
	}
	return Validate(c.Config)
}

func (s *Store) Save(c Committed) error  { return s.writeRecord("config.json", c) }
func (s *Store) Stage(c Committed) error { return s.writeRecord("pending.json", c) }
func (s *Store) writeRecord(name string, c Committed) error {
	if err := validateRecord(c); err != nil {
		return err
	}
	current, err := s.Load()
	if err == nil && current.StoreID != c.StoreID {
		return errors.New("configuration write cannot replace the durable store identity")
	}
	if err == nil && current.Startup != nil {
		old := current.Startup
		if c.Startup == nil || c.Startup.Selection.Generation < old.Selection.Generation || c.Startup.Selection.Generation == old.Selection.Generation && !c.Startup.Equal(*old) {
			return errors.New("configuration write cannot forget or alter adopted startup generation")
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > MaxRecordSize {
		return errors.New("committed configuration record exceeds its encoded size limit")
	}
	return atomicBytesChecked(filepath.Join(s.Dir, name), b, s.checkpoint)
}
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
	return AtomicWrite(path, b)
}

// AtomicWrite durably replaces a private file with already prepared bytes.
func AtomicWrite(path string, b []byte) error {
	return atomicBytesChecked(path, b, nil)
}

func atomicBytesChecked(path string, b []byte, check func(string) error) error {
	checkpoint := func(at string) error {
		if check != nil {
			return check(at)
		}
		return nil
	}
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
	if err = checkpoint("file-synced"); err != nil {
		return err
	}
	if err = checkpoint("before-rename"); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	if err = checkpoint("after-rename"); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return checkpoint("directory-synced")
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
