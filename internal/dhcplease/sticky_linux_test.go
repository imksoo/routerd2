//go:build linux

package dhcplease

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"golang.org/x/sys/unix"
)

func TestImportStickyPreservesWALHistoryAndExcludesRunningOrUsedService(t *testing.T) {
	work := t.TempDir()
	source := filepath.Join(work, "old.db")
	old, err := logstore.OpenDHCPStickyLog(source)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	now := time.Now().UTC()
	for _, item := range []struct{ mac, ip string }{
		{"02:00:00:00:00:01", "192.0.2.20"},
		{"02:00:00:00:00:02", "198.51.100.20"},
		{"02:00:00:00:00:03", "2001:db8::20"},
	} {
		if err := old.RecordLeaseEvent(t.Context(), "added", item.mac, item.ip, "old-client", 3, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := old.RecordLeaseEvent(t.Context(), "removed", item.mac, item.ip, "old-client", 3, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := old.List(t.Context(), logstore.DHCPStickyFilter{Now: now, IP: "192.0.2.20"})
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	if info, err := os.Stat(source + "-wal"); err != nil || info.Size() == 0 {
		t.Fatal("test requires uncheckpointed old history", err)
	}
	cfg := dhcpserverconfig.Normalize(dhcpserverconfig.Config{Resource: "lan", Interface: "lan0", ServerAddress: "192.0.2.1", StatePath: filepath.Join(work, "new", "dnsmasq.leases"), StickyHoldDays: 3, Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.10", End: "192.0.2.50"}})
	dir, lock, err := lockState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	defer lock.Close()
	if _, err := ImportSticky(t.Context(), cfg, source); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatal("running service was not excluded", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	// Importing active leases and sticky history are separate explicit initial
	// operations; the second operation must preserve the first file verbatim.
	active := fmt.Sprintf("%d 02:00:00:00:00:04 192.0.2.21 active-client *\n", now.Add(time.Hour).Unix())
	if _, err := Import(cfg, []byte(active)); err != nil {
		t.Fatal(err)
	}
	if count, err := ImportSticky(t.Context(), cfg, source); err != nil || count != 1 {
		t.Fatal("history import", count, err)
	}
	if data, err := os.ReadFile(cfg.StatePath); err != nil || string(data) != active {
		t.Fatal("history import changed lease file", err)
	}
	target, err := logstore.OpenDHCPStickyLogReadOnly(filepath.Join(filepath.Dir(cfg.StatePath), "dhcp-sticky.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	after, err := target.List(t.Context(), logstore.DHCPStickyFilter{Now: now})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("original timestamps or selected history changed", before, after, err)
	}
	if _, err := ImportSticky(t.Context(), cfg, source); err == nil {
		t.Fatal("existing history was overwritten")
	}
	if _, err := os.Stat(cfg.StatePath + ".owner"); !os.IsNotExist(err) {
		t.Fatal("history import manufactured HA ownership", err)
	}
	unchanged, err := old.List(t.Context(), logstore.DHCPStickyFilter{Now: now, IP: "192.0.2.20"})
	if err != nil || !reflect.DeepEqual(before, unchanged) {
		t.Fatal("source history changed", err)
	}
}
