//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The entire real process tree runs in a verified disposable namespace, with a
// second namespace as an independent packet peer. A required VM run cannot skip
// this test. No fake kernel, shell networking utility or Native constructor is
// used to apply the resource source program.
func TestDaemonResourceProgramLifecycleAndRecovery(t *testing.T) {
	if !isolateFixture(t, "TestDaemonResourceProgramLifecycleAndRecovery") {
		return
	}
	peerNS := fixturePeerNamespace(t, "program0", "peer0")
	peerHandle, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer peerHandle.Close()
	peer, err := peerHandle.LinkByName("peer0")
	if err != nil {
		t.Fatal(err)
	}
	for _, cidr := range []string{"192.0.2.1/24", "192.0.3.1/24", "198.51.100.1/24", "203.0.113.99/32"} {
		address, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := peerHandle.AddrAdd(peer, address); err != nil {
			t.Fatal(err)
		}
	}
	var listener *net.UDPConn
	if err := fixtureInsideNamespace(peerNS, func() error {
		var err error
		listener, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("203.0.113.99")})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	link, err := netlink.LinkByName("program0")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	foreignAddress, _ := netlink.ParseAddr("198.51.100.2/24")
	if err := netlink.AddrAdd(link, foreignAddress); err != nil {
		t.Fatal(err)
	}
	_, foreignPrefix, _ := net.ParseCIDR("198.19.77.0/24")
	foreignRoute := &netlink.Route{Dst: foreignPrefix, Gw: net.ParseIP("198.51.100.1"), LinkIndex: link.Attrs().Index, Table: 254, Priority: 201, Protocol: network.RouteProtocol}
	if err := netlink.RouteAdd(foreignRoute); err != nil {
		t.Fatal(err)
	}
	// The foreign route deliberately shares the worker's protocol number. Only
	// the ownership journal may authorize removal, not that protocol label.
	program, err := config.Parse([]byte(`schema_version: 2
resources:
  - id: tenant-route
    owner: tenant-address
    depends_on: [tenant-address]
    provenance:
      source: integration
      resource: StaticRoute/tenant
    route:
      destination: 203.0.113.0/24
      gateway: 192.0.2.1
      interface: program0
      metric: 200
  - id: permanent-address
    depends_on: [port]
    address:
      interface: program0
      cidr: 198.18.1.2/24
  - id: tenant-address
    depends_on: [port]
    address:
      interface: program0
      cidr: 192.0.2.2/24
  - id: port
    interface:
      name: program0
      mtu: 1440
`))
	if err != nil {
		t.Fatal(err)
	}
	fixture := startFixtureDaemon(t, program)
	readyState := func() control.State {
		t.Helper()
		var state control.State
		until(t, 5*time.Second, func() bool {
			return fixture.control.Do(context.Background(), "GET", "/v1/config", nil, &state) == nil && state.Healthy
		})
		return state
	}
	assertSource := func(state control.State, revision uint64, source config.Document) {
		t.Helper()
		want, _ := json.Marshal(source)
		got, _ := json.Marshal(state.Config)
		if state.Revision != revision || !state.Healthy || !bytes.Equal(want, got) {
			t.Fatalf("durable source changed: revision=%d healthy=%v\nsource=%s\nwant=%s", state.Revision, state.Healthy, got, want)
		}
	}
	assertPreserved := func() {
		t.Helper()
		if !resourceProgramAddress(t, link, "198.18.1.2/24") || !resourceProgramAddress(t, link, "198.51.100.2/24") || !resourceProgramRoute(t, "198.19.77.0/24", "198.51.100.1", link.Attrs().Index, 201) {
			t.Fatal("another declared owner or foreign kernel object was removed")
		}
	}
	assertManaged := func(cidr, gateway string, mtu int) {
		t.Helper()
		current, err := netlink.LinkByName("program0")
		if err != nil || current.Attrs().MTU != mtu || current.Attrs().Flags&net.FlagUp == 0 {
			t.Fatalf("interface projection mismatch: %+v %v", current, err)
		}
		if !resourceProgramAddress(t, link, cidr) || !resourceProgramRoute(t, "203.0.113.0/24", gateway, link.Attrs().Index, 200) {
			t.Fatalf("missing real address/route for %s through %s", cidr, gateway)
		}
		assertPreserved()
	}
	assertSource(readyState(), 1, program)
	assertManaged("192.0.2.2/24", "192.0.2.1", 1440)
	resourceProgramPacket(t, listener, "192.0.2.2")
	t.Log("source graph applied by real workers; dependent route carried a UDP round trip")

	updated := resourceProgramClone(t, program)
	resourceProgramResource(t, &updated, "port").Interface.MTU = 1420
	resourceProgramResource(t, &updated, "tenant-address").Address.CIDR = "192.0.3.2/24"
	resourceProgramResource(t, &updated, "tenant-route").Route.Gateway = "192.0.3.1"
	request := control.CommitRequest{ExpectedRevision: 1, RequestID: "program-update", Config: updated}
	if err := fixture.control.Do(context.Background(), "POST", "/v1/config/commit", request, nil); err != nil {
		t.Fatal(err)
	}
	assertSource(readyState(), 2, updated)
	assertManaged("192.0.3.2/24", "192.0.3.1", 1420)
	if resourceProgramAddress(t, link, "192.0.2.2/24") || resourceProgramRoute(t, "203.0.113.0/24", "192.0.2.1", link.Attrs().Index, 200) {
		t.Fatal("update retained the previous address or route next hop")
	}
	resourceProgramPacket(t, listener, "192.0.3.2")
	t.Log("source commit updated the interface, address and next hop; traffic uses the new address")

	for _, invalid := range []struct {
		name, dependency, diagnostic string
	}{{"cycle", "tenant-route", "cycle"}, {"missing", "absent-resource", "missing resource"}} {
		candidate := resourceProgramClone(t, updated)
		port := resourceProgramResource(t, &candidate, "port")
		port.Interface.MTU = 1300
		port.DependsOn = []string{invalid.dependency}
		resourceProgramResource(t, &candidate, "tenant-address").Address.CIDR = "192.0.9.2/24"
		request := control.CommitRequest{ExpectedRevision: 2, RequestID: "program-invalid-" + invalid.name, Config: candidate}
		err := fixture.control.Do(context.Background(), "POST", "/v1/config/commit", request, nil)
		if !localapi.IsHTTPStatus(err, 400) || !strings.Contains(err.Error(), invalid.diagnostic) {
			t.Fatalf("invalid %s graph did not fail before apply: %v", invalid.name, err)
		}
		assertSource(readyState(), 2, updated)
		assertManaged("192.0.3.2/24", "192.0.3.1", 1420)
		if resourceProgramAddress(t, link, "192.0.9.2/24") {
			t.Fatal("invalid graph changed the kernel before validation")
		}
	}
	t.Log("cyclic and missing dependencies left committed revision and kernel state unchanged")

	var status supervisor.Status
	if err := fixture.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	var victim supervisor.ModuleStatus
	for _, module := range status.Modules {
		if module.Name == "control" && module.Ready {
			victim = module
		}
	}
	if victim.PID <= 0 {
		t.Fatal("missing real control worker")
	}
	if err := syscall.Kill(victim.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	stopped := true
	t.Cleanup(func() {
		if stopped {
			_ = syscall.Kill(victim.PID, syscall.SIGCONT)
		}
	})
	_, managedPrefix, _ := net.ParseCIDR("203.0.113.0/24")
	if err := netlink.RouteDel(&netlink.Route{Dst: managedPrefix, Gw: net.ParseIP("192.0.3.1"), LinkIndex: link.Attrs().Index, Table: 254, Priority: 200, Protocol: network.RouteProtocol}); err != nil {
		t.Fatal(err)
	}
	managedAddress, _ := netlink.ParseAddr("192.0.3.2/24")
	if err := netlink.AddrDel(link, managedAddress); err != nil {
		t.Fatal(err)
	}
	if resourceProgramAddress(t, link, "192.0.3.2/24") || resourceProgramRoute(t, "203.0.113.0/24", "", link.Attrs().Index, 200) {
		t.Fatal("recovery fixture did not remove the real kernel projection")
	}
	if err := syscall.Kill(victim.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	stopped = false
	until(t, 15*time.Second, func() bool {
		var status supervisor.Status
		if fixture.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status) != nil || !status.Ready {
			return false
		}
		for _, module := range status.Modules {
			if module.Name == "control" && module.Ready && module.PID != victim.PID && module.Generation > victim.Generation {
				return true
			}
		}
		return false
	})
	assertSource(readyState(), 2, updated)
	assertManaged("192.0.3.2/24", "192.0.3.1", 1420)
	resourceProgramPacket(t, listener, "192.0.3.2")
	t.Log("replacement control worker reloaded committed source and repaired deleted kernel objects")

	remaining := resourceProgramClone(t, updated)
	remaining.Resources = remaining.Resources[:0]
	for _, resource := range updated.Resources {
		if resource.ID != "tenant-address" && resource.Owner != "tenant-address" {
			remaining.Resources = append(remaining.Resources, resource)
		}
	}
	request = control.CommitRequest{ExpectedRevision: 2, RequestID: "program-remove-owner", Config: remaining}
	if err := fixture.control.Do(context.Background(), "POST", "/v1/config/commit", request, nil); err != nil {
		t.Fatal(err)
	}
	assertSource(readyState(), 3, remaining)
	if resourceProgramAddress(t, link, "192.0.3.2/24") || resourceProgramRoute(t, "203.0.113.0/24", "", link.Attrs().Index, 200) {
		t.Fatal("removed source owner retained its address or dependent route")
	}
	assertPreserved()
	t.Log("owner subtree removal preserved another owner and foreign address/route using protocol 242")
}

func resourceProgramClone(t *testing.T, source config.Document) config.Document {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var result config.Document
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func resourceProgramResource(t *testing.T, source *config.Document, id string) *resourceprogram.Resource {
	t.Helper()
	for i := range source.Resources {
		if source.Resources[i].ID == id {
			return &source.Resources[i]
		}
	}
	t.Fatalf("source resource %s is missing", id)
	return nil
}

func resourceProgramAddress(t *testing.T, link netlink.Link, cidr string) bool {
	t.Helper()
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if address.IPNet.String() == cidr {
			return true
		}
	}
	return false
}

func resourceProgramRoute(t *testing.T, prefix, gateway string, index, metric int) bool {
	t.Helper()
	_, dst, err := net.ParseCIDR(prefix)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: dst, Table: 254}, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if int(route.Protocol) == network.RouteProtocol && route.Priority == metric && route.LinkIndex == index && (gateway == "" || route.Gw.String() == gateway) {
			return true
		}
	}
	return false
}

func resourceProgramPacket(t *testing.T, peer *net.UDPConn, source string) {
	t.Helper()
	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP(source)}, peer.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	payload := []byte("resource-program:" + source)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256)
	n, sender, err := peer.ReadFromUDP(buffer)
	if err != nil || sender == nil || sender.IP.String() != source || !bytes.Equal(buffer[:n], payload) {
		t.Fatalf("packet did not reach isolated peer with committed source address: n=%d sender=%v err=%v", n, sender, err)
	}
	if _, err := peer.WriteToUDP(buffer[:n], sender); err != nil {
		t.Fatal(err)
	}
	n, err = conn.Read(buffer)
	if err != nil || !bytes.Equal(buffer[:n], payload) {
		t.Fatalf("packet reply did not traverse committed kernel route: n=%d err=%v", n, err)
	}
}
