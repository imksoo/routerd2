//go:build linux

package dhcplease

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"golang.org/x/sys/unix"
)

func TestImportKeepsOriginalBindingsAndRefusesLiveOrExistingState(t *testing.T) {
	dir := t.TempDir()
	cfg := dhcpserverconfig.Normalize(dhcpserverconfig.Config{Resource: "lan", Interface: "lan0", ServerAddress: "192.0.2.1", StatePath: filepath.Join(dir, "dnsmasq.leases"), Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.10", End: "192.0.2.50"}})
	expiry := time.Now().Add(time.Hour).Unix()
	active := fmt.Sprintf("%d 02:00:00:00:00:01 192.0.2.20 client 01:02:00:00:00:00:01\n", expiry)
	data := []byte("1 02:00:00:00:00:02 192.0.2.21 expired *\nduid 00:01:00:01:aa:bb\n" + active)
	lock, err := os.OpenFile(cfg.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(cfg, data); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("running service was not rejected: %v", err)
	}
	if _, err := os.Stat(cfg.StatePath); !os.IsNotExist(err) {
		t.Fatalf("live-state refusal wrote leases: %v", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if n, err := Import(cfg, data); err != nil || n != 1 {
		t.Fatalf("import: count=%d, err=%v", n, err)
	}
	got, err := os.ReadFile(cfg.StatePath)
	if err != nil || string(got) != active {
		t.Fatalf("original identity or deadline changed: %q, %v", got, err)
	}
	if _, err := Import(cfg, []byte(strings.ReplaceAll(active, "192.0.2.20", "192.0.2.22"))); err == nil {
		t.Fatal("existing state was overwritten")
	}
	got, _ = os.ReadFile(cfg.StatePath)
	if string(got) != active {
		t.Fatal("refused import changed established state")
	}
	if _, err := os.Stat(cfg.StatePath + ".owner"); !os.IsNotExist(err) {
		t.Fatalf("import fabricated an HA owner: %v", err)
	}
}

func TestImportRejectsConflictsBeforeCreatingState(t *testing.T) {
	for _, tc := range []struct{ name, rows string }{
		{"foreign subnet", "02:00:00:00:00:01 198.51.100.20 client *"},
		{"bad client identity", "02:00:00:00:00:01 192.0.2.20 client zz:zz"},
		{"router address", "02:00:00:00:00:01 192.0.2.1 client *"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "new", "dnsmasq.leases")
			cfg := dhcpserverconfig.Config{Resource: "lan", Interface: "lan0", ServerAddress: "192.0.2.1", StatePath: state, Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.10", End: "192.0.2.50"}}
			if _, err := Import(cfg, []byte(fmt.Sprintf("%d %s\n", time.Now().Add(time.Hour).Unix(), tc.rows))); err == nil {
				t.Fatal("invalid migration accepted")
			}
			if _, err := os.Stat(filepath.Dir(state)); !os.IsNotExist(err) {
				t.Fatalf("invalid migration created state: %v", err)
			}
		})
	}
}
