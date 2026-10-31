//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/mobilityconfig"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

//go:embed testdata/sam-ipip-bgp-frr.yaml
var legacySAMIPIPBGPFRR []byte

func TestDaemonSAMIPIPBGPWithFRR(t *testing.T) { daemonSAMIPIPBGPWithFRR(t, false, false) }

func TestDaemonSAMIPIPBGPExpiryWithFRR(t *testing.T) { daemonSAMIPIPBGPWithFRR(t, true, false) }

func TestDaemonSAMIPIPBGPWithWireGuardFRR(t *testing.T) { daemonSAMIPIPBGPWithFRR(t, false, true) }

func TestDaemonSAMIPIPBGPExpiryWithWireGuardFRR(t *testing.T) { daemonSAMIPIPBGPWithFRR(t, true, true) }

// This is a real three-plane check: routerd2 receives an RRSet from an
// independent routerd HTTP server, uses it to create its kernel IPIP/BGP path,
// and forwards a terminal client's TCP and UDP through an independent FRR.
func daemonSAMIPIPBGPWithFRR(t *testing.T, expire, encrypted bool) {
	if !isolateFixtureTimeout(t, t.Name(), 90*time.Second) {
		return
	}
	oldRouterd := os.Getenv("OLD_ROUTERD")
	if oldRouterd == "" {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("OLD_ROUTERD must name an independent routerd RR")
		}
		t.Skip("set OLD_ROUTERD to run SAM transport interoperability")
	}
	frrPath := os.Getenv("ROUTERD2_FRR_BGPD")
	if frrPath == "" {
		frrPath = "/usr/lib/frr/bgpd"
	}
	if _, err := os.Stat(frrPath); err != nil {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("independent FRR bgpd required", err)
		}
		t.Skip("install FRR or set ROUTERD2_FRR_BGPD")
	}

	inner := netip.MustParsePrefix("10.255.0.0/24")
	slot := mobilityconfig.StableSAMTransportSlot(inner.String(), "leaf-a", "rr-a", 1<<(31-inner.Bits()))
	pair, err := mobilityconfig.SAMTransportSlotPrefix(inner, slot)
	if err != nil {
		t.Fatal(err)
	}
	leafInner, rrInner := pair.Addr(), pair.Addr().Next()
	if "leaf-a" > "rr-a" {
		leafInner, rrInner = rrInner, leafInner
	}
	leafOuter, rrOuter := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	tunnelLocal, tunnelRemote := leafOuter, rrOuter
	if encrypted {
		tunnelLocal, tunnelRemote = netip.MustParseAddr("10.20.0.31"), netip.MustParseAddr("10.20.0.2")
	}

	rr := fixturePeerNamespace(t, "wan0", "rr0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	wanCapture, lanCapture := healthPBRCapture(t, "wan0"), healthPBRCapture(t, "lan0")
	setup := func(ns netns.NsHandle, commands ...[]string) {
		t.Helper()
		if err := fixtureInsideNamespace(ns, func() error {
			for _, args := range commands {
				if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
					return fmt.Errorf("ip %v: %s: %w", args, out, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	work, err := os.MkdirTemp("/tmp", "samwg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	leafWGPrivate, leafWGPublic := "", ""
	rrWGPrivate, rrWGPublic := "", ""
	if encrypted {
		leafWGPrivate, leafWGPublic = samFixtureWireGuardKey(t)
		rrWGPrivate, rrWGPublic = samFixtureWireGuardKey(t)
		if err := os.WriteFile(filepath.Join(work, "leaf-wg.key"), []byte(leafWGPrivate+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "rr-wg.key"), []byte(rrWGPrivate+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rrTunnelLocal, rrTunnelRemote := rrOuter, leafOuter
	rrSetup := [][]string{
		[]string{"address", "add", "192.0.2.2/24", "dev", "rr0"},
		[]string{"address", "add", "203.0.113.50/32", "dev", "lo"},
	}
	if encrypted {
		rrTunnelLocal, rrTunnelRemote = netip.MustParseAddr("10.20.0.2"), netip.MustParseAddr("10.20.0.31")
		rrSetup = append(rrSetup,
			[]string{"link", "add", "wg-rr", "type", "wireguard"},
			[]string{"address", "add", "10.20.0.2/32", "dev", "wg-rr"},
			[]string{"link", "set", "wg-rr", "up"},
			[]string{"route", "add", "10.20.0.31/32", "dev", "wg-rr"},
		)
	}
	rrSetup = append(rrSetup,
		[]string{"link", "add", "rrtun0", "type", "ipip", "local", rrTunnelLocal.String(), "remote", rrTunnelRemote.String(), "ttl", "64"},
		[]string{"link", "set", "rrtun0", "up"},
		[]string{"address", "add", rrInner.String() + "/31", "dev", "rrtun0"},
	)
	setup(rr, rrSetup...)
	if encrypted {
		if err := fixtureInsideNamespace(rr, func() error {
			return exec.Command("wg", "set", "wg-rr", "listen-port", "51820", "private-key", filepath.Join(work, "rr-wg.key"), "peer", leafWGPublic, "endpoint", "192.0.2.1:51820", "allowed-ips", "10.20.0.31/32", "persistent-keepalive", "5").Run()
		}); err != nil {
			t.Fatalf("configure independent RR WireGuard peer: %v", err)
		}
	}
	setup(client,
		[]string{"address", "add", "198.18.0.2/24", "dev", "client0"},
		[]string{"route", "add", "default", "via", "198.18.0.1"},
	)

	controlToken, joinToken := filepath.Join(work, "control-token"), filepath.Join(work, "join-token")
	for path, value := range map[string]string{controlToken: "sam-fixture-control", joinToken: "sam-fixture-join"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rrConfig := filepath.Join(work, "rr.yaml")
	if err := os.WriteFile(rrConfig, []byte(samFixtureRRConfig(joinToken, rrWGPublic, filepath.Join(work, "rr-wg.key"))), 0600); err != nil {
		t.Fatal(err)
	}
	rrLog, err := os.Create(filepath.Join(work, "rr.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rrLog.Close() })
	rrContext, cancelRR := context.WithCancel(context.Background())
	rrCommand := exec.CommandContext(rrContext, oldRouterd, "serve", "--sandbox", "--root", filepath.Join(work, "rr-state"), "--config", rrConfig, "--http-listen", "192.0.2.2:16543", "--http-allow-cidr", leafOuter.String()+"/32", "--http-token-file", controlToken, "--controllers", "none", "--graceful-stop-timeout", "0")
	rrCommand.Stdout, rrCommand.Stderr = rrLog, rrLog
	if err := fixtureInsideNamespace(rr, rrCommand.Start); err != nil {
		cancelRR()
		t.Fatal(err)
	}
	rrDone := make(chan error, 1)
	go func() { rrDone <- rrCommand.Wait() }()
	rrStopped := false
	stopRR := func() {
		if rrStopped {
			return
		}
		rrStopped = true
		cancelRR()
		select {
		case err := <-rrDone:
			if err != nil && !strings.Contains(err.Error(), "signal: killed") {
				t.Errorf("independent RR exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = rrCommand.Process.Kill()
			<-rrDone
			t.Error("independent RR did not stop")
		}
	}
	t.Cleanup(func() {
		stopRR()
		if t.Failed() {
			data, _ := os.ReadFile(rrLog.Name())
			t.Logf("independent RR: %s", data)
		}
	})
	until(t, 10*time.Second, func() bool {
		return fixtureInsideNamespace(rr, func() error {
			connection, err := net.DialTimeout("tcp", "192.0.2.2:16543", 100*time.Millisecond)
			if err == nil {
				_ = connection.Close()
			}
			return err
		}) == nil
	})

	wan, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	dhcpHosts := filepath.Join(work, "dhcp-hosts")
	if err := os.WriteFile(dhcpHosts, []byte(wan.Attrs().HardwareAddr.String()+",192.0.2.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = startFixtureDNSMasq(t, rr, "rr0", []string{"--port=0", "--dhcp-range=192.0.2.1,192.0.2.20,255.255.255.0,60s", "--dhcp-hostsfile=" + dhcpHosts})
	frrRoot := startSAMFixtureFRR(t, rr, frrPath, leafInner)
	startSAMFixtureTerminal(t, rr)

	claim := api.SAMEnrollmentClaimSpec{
		PolicyRef:     "SAMEnrollmentPolicy/leaves",
		RRSetRef:      "SAMRRSet/rrs",
		LeafID:        "leaf-a",
		JoinAudience:  "sam-ipip-fixture",
		JoinNonce:     "sam-ipip-fixture-nonce",
		JoinTimestamp: time.Now().UTC().Format(time.RFC3339Nano),
		TunnelAddress: leafInner.String() + "/32",
		Endpoint:      "192.0.2.1",
		Mobility:      api.SAMEnrollmentClaimMobilitySpec{OwnedAddresses: []string{"203.0.113.51/32"}},
		BGP:           api.SAMEnrollmentClaimBGPSpec{ASN: 64512, RouterID: "192.0.2.1"},
	}
	if encrypted {
		claim.WireGuard = api.SAMEnrollmentClaimWireGuardSpec{PublicKey: leafWGPublic, Endpoint: "192.0.2.1:51820", AllowedIPs: []string{"10.20.0.31/32"}, PersistentKeepalive: 5}
	}
	claim.JoinHMAC = samenrollment.JoinHMAC([]byte("sam-fixture-join"), claim)
	legacy := strings.NewReplacer(
		"__JOIN_TIMESTAMP__", claim.JoinTimestamp,
		"__JOIN_HMAC__", claim.JoinHMAC,
		"__LEAF_INNER__", leafInner.String(),
		"__RR_ENDPOINT__", "http://192.0.2.2:16543",
		"__CONTROL_TOKEN__", controlToken,
	).Replace(string(legacySAMIPIPBGPFRR))
	if encrypted {
		legacy = strings.ReplaceAll(legacy, "encryption: none", "encryption: wireguard")
		legacy = strings.ReplaceAll(legacy, "underlayInterface: wan", "underlayInterface: wg-sam")
		legacy = strings.ReplaceAll(legacy, "localEndpointFrom: {resource: DHCPv4Client/wan-lease, field: currentAddress}", "localEndpoint: 10.20.0.31")
		legacy = strings.Replace(legacy, "      wireGuard: __LEAF_WG_CLAIM__", fmt.Sprintf("      wireGuard: {publicKey: %q, endpoint: %q, allowedIPs: [10.20.0.31/32], persistentKeepalive: 5}", leafWGPublic, "192.0.2.1:51820"), 1)
		legacy = strings.Replace(legacy, "  - apiVersion: mobility.routerd.net/v1alpha1\n    kind: SAMTransportProfile", fmt.Sprintf(`  - apiVersion: net.routerd.net/v1alpha1
    kind: WireGuardInterface
    metadata: {name: wg-sam}
    spec: {ifname: wg-sam, selfNodeRef: leaf-a, privateKeyFile: %q, listenPort: 51820, mtu: 1420, peersFrom: [{resource: SAMRRSet/rrs}]}
  - apiVersion: net.routerd.net/v1alpha1
    kind: IPv4StaticAddress
    metadata: {name: wg-sam-address}
    spec: {interface: WireGuardInterface/wg-sam, address: 10.20.0.31/32}
  - apiVersion: mobility.routerd.net/v1alpha1
    kind: SAMTransportProfile`, filepath.Join(work, "leaf-wg.key")), 1)
	} else {
		legacy = strings.Replace(legacy, "interfaces: [Interface/lan, WireGuardInterface/wg-sam]", "interfaces: [Interface/lan]", 1)
		legacy = strings.Replace(legacy, "      wireGuard: __LEAF_WG_CLAIM__\n", "", 1)
	}
	document, err := legacyconfig.Compile([]byte(legacy))
	if err != nil {
		t.Fatal("compile SAM transport fixture", err)
	}
	plan, err := config.Compile(document.Document)
	if err != nil {
		t.Fatal("compile native SAM transport fixture", err)
	}
	var samModule, bgpModule string
	for _, service := range plan.Native.Services {
		if service.SAMClient != nil {
			samModule = service.ModuleName()
		}
		if service.BGP != nil {
			bgpModule = service.ModuleName()
		}
	}
	if samModule == "" || bgpModule == "" {
		t.Fatal("fixture did not produce SAM client and BGP services")
	}
	leafRoot := filepath.Join(work, "leaf")
	if err := os.Mkdir(leafRoot, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := startFixtureDaemonAt(t, document.Document, leafRoot)

	var admitted samclientconfig.Snapshot
	var lastSAM runtimeplan.InstanceStatus
	var lastSAMError error
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last SAM service observation: status=%+v request_error=%v", lastSAM, lastSAMError)
		}
	})
	until(t, 20*time.Second, func() bool {
		var instance runtimeplan.InstanceStatus
		if err := fixture.control.Do(t.Context(), "GET", "/v1/services/"+samModule, nil, &instance); err != nil {
			lastSAMError = err
			return false
		}
		lastSAM = instance
		if err := json.Unmarshal(instance.Snapshot, &admitted); err != nil {
			lastSAMError = err
			return false
		}
		lastSAMError = nil
		return admitted.Phase == "ready" && admitted.RRSet != nil && len(admitted.RRSet.Nodes) == 1 && admitted.RRSet.Nodes[0].NodeRef == "rr-a"
	})
	tunnel := samFixtureTunnel(t, tunnelLocal, tunnelRemote, leafInner)
	showFRR := func(prefix string) bool {
		output, err := exec.Command("vtysh", "--vty_socket", frrRoot, "-d", "bgpd", "-c", "show bgp ipv4 unicast "+prefix+" json").Output()
		var view struct {
			Paths []json.RawMessage `json:"paths"`
		}
		return err == nil && json.Unmarshal(output, &view) == nil && len(view.Paths) != 0
	}
	until(t, 25*time.Second, func() bool { return showFRR("198.18.0.0/24") })
	until(t, 25*time.Second, func() bool { return showFRR("203.0.113.51/32") })
	destination := netip.MustParsePrefix("203.0.113.50/32")
	until(t, 20*time.Second, func() bool { return samFixtureRoute(destination, tunnel, rrInner) })
	if encrypted {
		view, err := control.NewNetworkClient(filepath.Join(leafRoot, "run")).Observe(t.Context())
		if err != nil {
			t.Fatalf("observe WireGuard network: %v", err)
		}
		if len(view.Snapshot.WireGuard) != 1 {
			t.Fatalf("WireGuard device count: %+v", view.Snapshot.WireGuard)
		}
		device := view.Snapshot.WireGuard[0]
		if len(device.Peers) != 1 || device.Peers[0].PublicKey != rrWGPublic || device.Peers[0].LastHandshake.IsZero() {
			t.Fatalf("independent RR WireGuard peer is not handshaking: %+v", device)
		}
	}
	forward := func() error {
		return fixtureInsideNamespace(client, func() error {
			output, err := exec.Command("python3", "-c", `import socket
for kind,port in ((socket.SOCK_STREAM,18080),(socket.SOCK_DGRAM,18081)):
 s=socket.socket(socket.AF_INET,kind);s.settimeout(2);s.connect(('203.0.113.50',port))
 if kind==socket.SOCK_DGRAM:s.send(b'SAM')
 assert s.recv(10)==b'SAM'
 s.close()`).CombinedOutput()
			if err != nil {
				return fmt.Errorf("NAT-less client forwarding: %s: %w", strings.TrimSpace(string(output)), err)
			}
			return nil
		})
	}
	if err := forward(); err != nil {
		t.Fatal(err)
	}
	until(t, 3*time.Second, func() bool {
		encapsulationSeen := samFixtureIPIPCapture(wanCapture, "192.0.2.1", "192.0.2.2")
		if encrypted {
			encapsulationSeen = wanCapture.count("192.0.2.1", "192.0.2.2", unix.IPPROTO_UDP, 51820) > 0 && samFixtureWireGuardHandshake("wg-sam")
		}
		return encapsulationSeen &&
			lanCapture.count("198.18.0.2", "203.0.113.50", 6, 18080) > 0 &&
			lanCapture.countSourcePort("203.0.113.50", "198.18.0.2", 6, 18080) > 0 &&
			lanCapture.count("198.18.0.2", "203.0.113.50", 17, 18081) > 0 &&
			lanCapture.countSourcePort("203.0.113.50", "198.18.0.2", 17, 18081) > 0
	})

	if !expire {
		local := localapi.NewClient(filepath.Join(leafRoot, "run", "services", strings.TrimPrefix(samModule, "service.")+".sam.sock"))
		var withdrawn samclientconfig.Snapshot
		if err := local.Do(t.Context(), "POST", "/v1/revoke", nil, &withdrawn); err != nil {
			var current runtimeplan.InstanceStatus
			statusErr := fixture.control.Do(t.Context(), "GET", "/v1/services/"+samModule, nil, &current)
			t.Fatalf("revoke stopped working after forwarding: %v; control status=%+v status_error=%v", err, current, statusErr)
		}
		if withdrawn.Phase != "revoked" || withdrawn.RRSet != nil {
			t.Fatalf("local withdrawal retained transport topology: %+v", withdrawn)
		}
		until(t, 20*time.Second, func() bool { return !showFRR("203.0.113.51/32") })
		until(t, 15*time.Second, func() bool { return !samFixtureRoute(destination, tunnel, rrInner) && samFixtureTunnelAbsent(tunnel) })
		if err := forward(); err == nil {
			t.Fatal("revoked SAM path still forwards")
		}
		client := controlapi.NewHTTPClient("http://192.0.2.2:16543").WithBearerToken("sam-fixture-control")
		if _, err := client.GetSAMEnrollmentTopology(t.Context(), controlapi.SAMEnrollmentTopologyGetRequest{Name: "rrs", ClaimRef: "SAMEnrollmentClaim/leaf", ClaimDigest: samenrollment.ClaimDigest(claim), ClaimIdentityDigest: samenrollment.ClientIdentityDigest(claim)}); err == nil || !strings.Contains(err.Error(), "revoked") {
			t.Fatalf("independent RR retained a revoked topology: %v", err)
		}
		t.Log("independent RR admission -> routerd2 SAM-owned /32 advertisement -> IPIP/BGP -> NAT-less TCP/UDP passed; explicit revoke withdrew kernel tunnel and FIB route")
		return
	}

	stopRR()
	until(t, 20*time.Second, func() bool { return !showFRR("203.0.113.51/32") })
	until(t, 20*time.Second, func() bool { return !samFixtureRoute(destination, tunnel, rrInner) && samFixtureTunnelAbsent(tunnel) })
	if err := forward(); err == nil {
		t.Fatal("expired SAM path still forwards after its RR disappeared")
	}
	t.Log("independent RR admission -> routerd2 SAM-owned /32 advertisement -> IPIP/BGP -> NAT-less TCP/UDP passed; original RR admission expiry withdrew kernel tunnel and FIB route")
}

func samFixtureRRConfig(joinToken, rrWGPublic, rrWGPrivatePath string) string {
	nodeWG, policyWG, wireguardInterface, encryption, underlay, localEndpoint := "", "", "", "none", "underlay", "192.0.2.2"
	if rrWGPublic != "" {
		nodeWG = fmt.Sprintf(`, wireGuard: {publicKey: %q, endpoint: "192.0.2.2:51820", transportEndpoint: "10.20.0.2", allowedIPs: ["10.20.0.2/32"], persistentKeepalive: 5}`, rrWGPublic)
		policyWG = "        wireGuard: {interface: wg-rr, endpointPrefixes: [192.0.2.0/24], allowedExtraIPPrefixes: [10.20.0.0/24], persistentKeepalive: 5}\n"
		wireguardInterface = fmt.Sprintf(`    - apiVersion: net.routerd.net/v1alpha1
      kind: WireGuardInterface
      metadata: {name: wg-rr}
      spec: {ifname: wg-rr, selfNodeRef: rr-a, privateKeyFile: %q, listenPort: 51820, peersFrom: [{resource: SAMEnrollmentPolicy/leaves}]}
`, rrWGPrivatePath)
		encryption, underlay, localEndpoint = "wireguard", "wg-rr", "10.20.0.2"
	}
	config := fmt.Sprintf(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: sam-fixture-rr}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: underlay}
      spec: {ifname: rr0, managed: false}
    - apiVersion: net.routerd.net/v1alpha1
      kind: BGPRouter
      metadata: {name: fabric}
      spec: {asn: 64513, routerID: 192.0.2.2}
%s    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMNodeSet
      metadata: {name: rrs}
      spec: {nodes: [{nodeRef: rr-a, routeReflector: true, samEndpoint: 192.0.2.2%s}]}
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMTransportProfile
      metadata: {name: rr}
      spec:
        selfNodeRef: rr-a
        mode: ipip
        encryption: %s
        addressingMode: pair-stable
        innerPrefix: 10.255.0.0/24
        underlayInterface: %s
        localEndpoint: %s
        peersFrom: [{resource: SAMEnrollmentPolicy/leaves}]
        bgp: {routerRef: BGPRouter/fabric, peerASN: 64512, routeReflectorClient: true, importPolicy: {allowedPrefixes: [198.18.0.0/24]}}
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMEnrollmentPolicy
      metadata: {name: leaves}
      spec:
        transportProfileRef: SAMTransportProfile/rr
        rrSetRef: SAMRRSet/rrs
        rrNodeSetRef: SAMNodeSet/rrs
        joinTokenFrom: {file: %q}
        joinAudience: sam-ipip-fixture
        allowedLeafIDs: {pattern: ^leaf-a$}
        tunnelAddressPrefixes: [10.255.0.0/24]
        endpointPrefixes: [192.0.2.0/24]
        mobilityPrefixes: [203.0.113.0/24]
        ttl: 8s
%s
`, wireguardInterface, nodeWG, encryption, underlay, localEndpoint, joinToken, policyWG)
	return config
}

func startSAMFixtureFRR(t *testing.T, namespace netns.NsHandle, frrPath string, leaf netip.Addr) string {
	t.Helper()
	root, err := os.MkdirTemp("", "routerd-sam-frr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if output, err := exec.Command("chown", "frr:frr", root).CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	write := func(name, contents string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	zebraConfig := write("zebra.conf", "hostname independent\nlog stdout\n", 0644)
	zebraLog, err := os.Create(filepath.Join(root, "zebra.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zebraLog.Close() })
	zebraSocket := filepath.Join(root, "zserv.api")
	zebra := exec.Command(filepath.Join(filepath.Dir(frrPath), "zebra"), "-u", "frr", "-g", "frr", "-z", zebraSocket, "--vty_socket", root, "-i", filepath.Join(root, "zebra.pid"), "-f", zebraConfig, "--log", "stdout")
	zebra.Stdout, zebra.Stderr = zebraLog, zebraLog
	if err := fixtureInsideNamespace(namespace, zebra.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zebra.Process.Kill(); _ = zebra.Wait() })
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(zebraLog.Name())
			t.Logf("FRR zebra: %s", data)
		}
	})
	until(t, 5*time.Second, func() bool {
		info, err := os.Stat(zebraSocket)
		return err == nil && info.Mode()&os.ModeSocket != 0
	})
	bgpConfig := write("bgpd.conf", fmt.Sprintf("hostname independent\nlog stdout critical\nrouter bgp 64513\n bgp router-id 192.0.2.2\n no bgp ebgp-requires-policy\n no bgp network import-check\n neighbor %s remote-as 64512\n address-family ipv4 unicast\n  network 203.0.113.50/32\n  neighbor %s activate\n exit-address-family\n", leaf, leaf), 0600)
	bgpLog, err := os.Create(filepath.Join(root, "bgpd.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bgpLog.Close() })
	bgpd := exec.Command(frrPath, "-S", "-z", zebraSocket, "-p", "179", "--vty_socket", root, "-i", filepath.Join(root, "bgpd.pid"), "-f", bgpConfig, "--log", "stdout")
	bgpd.Stdout, bgpd.Stderr = bgpLog, bgpLog
	if err := fixtureInsideNamespace(namespace, bgpd.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bgpd.Process.Kill(); _ = bgpd.Wait() })
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(bgpLog.Name())
			t.Logf("FRR bgpd: %s", data)
		}
	})
	return root
}

func startSAMFixtureTerminal(t *testing.T, namespace netns.NsHandle) {
	t.Helper()
	server := exec.Command("python3", "-u", "-c", `import socket, threading
def udp():
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('203.0.113.50',18081))
 while True:
  data,addr=s.recvfrom(1024);s.sendto(data,addr)
threading.Thread(target=udp,daemon=True).start()
s=socket.socket();s.bind(('203.0.113.50',18080));s.listen()
while True:
 c,_=s.accept();c.sendall(b'SAM');c.close()`)
	if err := fixtureInsideNamespace(namespace, server.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
}

func samFixtureTunnel(t *testing.T, outerLocal, outerRemote, localInner netip.Addr) string {
	t.Helper()
	var name string
	until(t, 20*time.Second, func() bool {
		links, err := netlink.LinkList()
		if err != nil {
			return false
		}
		for _, link := range links {
			tunnel, ok := link.(*netlink.Iptun)
			if !ok || !tunnel.Local.Equal(net.IP(outerLocal.AsSlice())) || !tunnel.Remote.Equal(net.IP(outerRemote.AsSlice())) {
				continue
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
			if err != nil {
				continue
			}
			for _, address := range addresses {
				if address.IPNet != nil && address.IPNet.String() == netip.PrefixFrom(localInner, 31).String() {
					name = link.Attrs().Name
					return true
				}
			}
		}
		return false
	})
	return name
}

func samFixtureTunnelAbsent(name string) bool {
	_, err := netlink.LinkByName(name)
	return err != nil
}

func samFixtureWireGuardKey(t *testing.T) (string, string) {
	t.Helper()
	private, err := exec.Command("wg", "genkey").Output()
	if err != nil {
		t.Fatal("generate isolated WireGuard private key:", err)
	}
	publicCommand := exec.Command("wg", "pubkey")
	publicCommand.Stdin = bytes.NewReader(private)
	public, err := publicCommand.Output()
	if err != nil {
		t.Fatal("derive isolated WireGuard public key:", err)
	}
	return strings.TrimSpace(string(private)), strings.TrimSpace(string(public))
}

func samFixtureWireGuardHandshake(ifname string) bool {
	output, err := exec.Command("wg", "show", ifname, "latest-handshakes").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] != "0" {
			return true
		}
	}
	return false
}

func samFixtureRoute(destination netip.Prefix, tunnel string, gateway netip.Addr) bool {
	link, err := netlink.LinkByName(tunnel)
	if err != nil {
		return false
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: &net.IPNet{IP: net.IP(destination.Addr().AsSlice()), Mask: net.CIDRMask(destination.Bits(), 32)}}, netlink.RT_FILTER_DST)
	if err != nil {
		return false
	}
	for _, route := range routes {
		if route.LinkIndex == link.Attrs().Index && route.Gw.Equal(net.IP(gateway.AsSlice())) && int(route.Protocol) == network.RouteProtocol {
			return true
		}
	}
	return false
}

func samFixtureIPIPCapture(c *healthPBRCaptureState, source, destination string) bool {
	from, to := net.ParseIP(source).To4(), net.ParseIP(destination).To4()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, packet := range c.packets {
		b := packet.data
		if len(b) >= 34 && b[12] == 8 && b[13] == 0 && b[14]>>4 == 4 && b[23] == unix.IPPROTO_IPIP && bytes.Equal(b[26:30], from) && bytes.Equal(b[30:34], to) {
			return true
		}
	}
	return false
}
