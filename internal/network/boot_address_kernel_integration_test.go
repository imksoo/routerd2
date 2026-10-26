//go:build linux && integration

package network

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestIsolatedBootAddressLifetime(t *testing.T) {
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ROUTERD2_BOOT_ADDRESS_CHILD") != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestIsolatedBootAddressLifetime$", "-test.v", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), "ROUTERD2_BOOT_ADDRESS_CHILD=1", "ROUTERD2_BOOT_ADDRESS_PARENT="+parent)
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
		output, err := cmd.CombinedOutput()
		if err != nil {
			if (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
				t.Skipf("isolated namespaces unavailable: %v", err)
			}
			t.Fatalf("boot lifetime kernel fixture: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	if prior := os.Getenv("ROUTERD2_BOOT_ADDRESS_PARENT"); prior == "" || prior == parent {
		t.Fatal("refusing network mutation outside isolated namespace")
	}
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "boot0"}, PeerName: "bootpeer"}); err != nil {
		t.Fatal(err)
	}
	link, err := netlink.LinkByName("boot0")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	foreign, _ := netlink.ParseAddr("192.0.2.201/32")
	if err := netlink.AddrAdd(link, foreign); err != nil {
		t.Fatal(err)
	}
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	expiry := uint64(clock.BoottimeNanoseconds) + uint64(6*time.Second)
	address := "192.0.2.200/24"
	d := Desired{Interfaces: []Interface{{Name: "boot0", Addresses: []string{address}, AddressOptions: map[string]AddressOptions{address: {BootLifetime: BootLifetime{BootID: clock.BootID, PreferredNanos: expiry, ValidNanos: expiry}}}}}}
	d.Interfaces[0].Addresses = append(d.Interfaces[0].Addresses, "192.0.2.202/32")
	d.Interfaces[0].AddressOptions["192.0.2.202/32"] = AddressOptions{BootLifetime: BootLifetime{BootID: clock.BootID, PreferredNanos: InfiniteNanos, ValidNanos: InfiniteNanos}}
	opts := Options{StatePath: filepath.Join(t.TempDir(), "network.json")}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		e.Close()
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	e, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		e.Close()
		t.Fatal(err)
	}
	addresses, err := netlink.AddrList(link, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	found, infinite := false, false
	for _, a := range addresses {
		if a.IP.String() == "192.0.2.202" {
			infinite = true
			if a.ValidLft != int(^uint32(0)) || a.PreferedLft != int(^uint32(0)) {
				t.Fatal("boot infinity was encoded as a finite kernel lifetime", a)
			}
		}
		if a.IP.String() == "192.0.2.200" {
			found = true
			if a.ValidLft > 5 || a.ValidLft <= 0 {
				t.Fatal("writer restart reset original lifetime", a.ValidLft)
			}
		}
	}
	if !found || !infinite {
		t.Fatal("finite/infinite lease address missing after writer restart")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(7 * time.Second)
	for {
		addresses, err = netlink.AddrList(link, unix.AF_INET)
		if err != nil {
			t.Fatal(err)
		}
		owned, kept, infinite := false, false, false
		for _, a := range addresses {
			owned = owned || a.IP.String() == "192.0.2.200"
			kept = kept || a.IP.String() == "192.0.2.201"
			infinite = infinite || a.IP.String() == "192.0.2.202"
		}
		if !kept || !infinite {
			t.Fatal("foreign or explicitly infinite address removed")
		}
		if !owned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kernel did not expire address with both writers closed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	e, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	addresses, err = netlink.AddrList(link, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if a.IP.String() == "192.0.2.200" {
			t.Fatal("expired original intent recreated lease")
		}
	}
	t.Log("finite kernel IPv4 address expired autonomously while writers were closed; journal restart and replay preserved original BOOTTIME deadline and foreign static address")
	t.Log("ROUTERD_BOOT_ADDRESS_TEST_PASS")
}
