//go:build linux && integration

package integration

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

//go:embed testdata/ingress.yaml
var legacyIngress []byte

// The independent HTTPS servers and client share an isolated LAN with static
// fixture addresses. The deployed ingress request, intervals, thresholds and
// VIP tracking remain unchanged. DHCP, DNS, BGP, SAM, a second router and physical
// deployment are outside this acceptance.
func TestDaemonImportedIngressHTTPS(t *testing.T) {
	if !isolateFixtureTimeout(t, t.Name(), 150*time.Second) {
		return
	}
	peer := fixturePeerNamespace(t, "lan0", "peer0")
	var client netns.NsHandle
	if err := fixtureInsideNamespace(peer, func() error {
		for _, args := range [][]string{
			{"link", "add", "switch0", "type", "bridge"},
			{"link", "set", "switch0", "up"},
			{"link", "set", "peer0", "master", "switch0"},
			{"address", "add", "192.0.2.53/24", "dev", "switch0"},
			{"address", "add", "192.0.2.62/24", "dev", "switch0"},
			{"address", "add", "192.0.2.63/24", "dev", "switch0"},
			{"address", "add", "192.0.2.64/24", "dev", "switch0"},
		} {
			if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("independent LAN: %w: %s", err, out)
			}
		}
		client = fixturePeerNamespace(t, "client-edge", "client0")
		for _, args := range [][]string{{"link", "set", "client-edge", "master", "switch0"}, {"link", "set", "client-edge", "up"}} {
			if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("client LAN: %w: %s", err, out)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixtureInsideNamespace(client, func() error {
		// This host route intentionally sends a direct-backend connection through
		// the product too. Ordinary same-LAN traffic would bypass its NAT rules.
		for _, args := range [][]string{{"address", "add", "192.0.2.100/24", "dev", "client0"}, {"route", "add", "192.0.2.62/32", "via", "192.0.2.38"}} {
			if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("client setup: %w: %s", err, out)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	capture := healthPBRCapture(t, "lan0")
	work := t.TempDir()
	statusPath := filepath.Join(work, "status.json")
	setStatus := func(status [3]int) {
		t.Helper()
		raw, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statusPath+".new", raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(statusPath+".new", statusPath); err != nil {
			t.Fatal(err)
		}
	}
	setStatus([3]int{503, 204, 503})
	key, cert := filepath.Join(work, "server.key"), filepath.Join(work, "server.crt")
	if out, err := exec.Command("openssl", "req", "-config", "/dev/null", "-x509", "-newkey", "ed25519", "-nodes", "-days", "1", "-keyout", key, "-out", cert, "-subj", "/CN=k8s-api.lain.local", "-addext", "subjectAltName=DNS:k8s-api.lain.local").CombinedOutput(); err != nil {
		t.Fatalf("HTTPS certificate: %v: %s", err, out)
	}
	logPath, readyPath := filepath.Join(work, "peer.jsonl"), filepath.Join(work, "ready")
	peerLog, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command("python3", "-u", "-c", ingressHTTPSPeer, statusPath, cert, key, readyPath)
	server.Stdout, server.Stderr = peerLog, peerLog
	if err := fixtureInsideNamespace(peer, server.Start); err != nil {
		peerLog.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
		_ = peerLog.Close()
		if t.Failed() {
			raw, _ := os.ReadFile(logPath)
			t.Logf("HTTPS peer: %s", raw)
		}
	})
	until(t, 5*time.Second, func() bool { _, err := os.Stat(readyPath); return err == nil })
	source, policy := filepath.Join(work, "source.yaml"), filepath.Join(work, "policy.yaml")
	if err := os.WriteFile(source, legacyIngress, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte("schema_version: 1\nvrrp:\n  VirtualAddress/k8s-routerd-vip:\n    source_address: 192.0.2.38\n    vip_interface: ingress-ha\n"), 0600); err != nil {
		t.Fatal(err)
	}
	migrate := exec.CommandContext(t.Context(), filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "migrate", "--policy", policy, source)
	var diagnostic bytes.Buffer
	migrate.Stderr = &diagnostic
	raw, err := migrate.Output()
	if err != nil {
		t.Fatal("migrate", err, diagnostic.String())
	}
	program, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	healthNames := map[string]string{}
	vrrpName := ""
	for _, resource := range program.Resources {
		if resource.Service == nil {
			continue
		}
		if c := resource.Service.Health; c != nil {
			healthNames[c.Target] = resource.Service.ModuleName()
		}
		if resource.Service.VRRP != nil {
			vrrpName = resource.Service.ModuleName()
		}
	}
	if len(healthNames) != 3 || vrrpName == "" {
		t.Fatal("migrate omitted declared probes or VIP election", healthNames, vrrpName)
	}
	f := startFixtureDaemon(t, program)
	snapshot := func(module string) json.RawMessage {
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(t.Context(), "GET", "/v1/services/"+module, nil, &status); err != nil {
			return nil
		}
		return status.Snapshot
	}
	healthState := func(address string) health.Snapshot {
		var s health.Snapshot
		if raw := snapshot(healthNames[address]); len(raw) > 0 {
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal("Health observation", err)
			}
		}
		return s
	}
	waitBackend := func(address string, active bool) {
		t.Helper()
		until(t, 18*time.Second, func() bool {
			s := healthState(address)
			if s.BootID == "" || s.Stopped || s.Active != active {
				return false
			}
			if active {
				return s.State.ConsecutivePassed >= 2
			}
			return s.State.ConsecutiveFailed >= 2
		})
	}
	waitPriority := func(priority int, after time.Time) {
		t.Helper()
		until(t, 25*time.Second, func() bool {
			var s vrrp.Snapshot
			if json.Unmarshal(snapshot(vrrpName), &s) != nil || !s.Active || s.Priority != priority {
				return false
			}
			capture.mu.Lock()
			defer capture.mu.Unlock()
			for _, p := range capture.packets {
				b := p.data
				if p.at.Before(after) || len(b) < 38 || binary.BigEndian.Uint16(b[12:14]) != 0x0800 || b[23] != 112 || !bytes.Equal(b[26:30], net.ParseIP("192.0.2.38").To4()) {
					continue
				}
				offset := 14 + int(b[14]&15)*4
				if len(b) > offset+2 && b[offset+1] == 50 && int(b[offset+2]) == priority {
					return true
				}
			}
			return false
		})
	}
	type response struct {
		Backend    string `json:"backend"`
		Peer       string `json:"peer"`
		Refused    bool   `json:"refused"`
		Reset      bool   `json:"reset"`
		SourcePort int    `json:"source_port"`
	}
	request := func(destination string) response {
		t.Helper()
		var result response
		if err := fixtureInsideNamespace(client, func() error {
			out, err := exec.CommandContext(t.Context(), "python3", "-c", ingressHTTPSClient, destination).CombinedOutput()
			if err != nil {
				return fmt.Errorf("client HTTPS to %s: %w: %s", destination, err, out)
			}
			return json.Unmarshal(out, &result)
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	checkVIP := func(backend string) {
		t.Helper()
		result := request("192.0.2.248")
		if result.Backend != backend || result.Refused {
			t.Fatal("wrong backend", result, "want", backend)
		}
		addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
		if err != nil {
			t.Fatal(err)
		}
		owned := false
		for _, address := range addresses {
			owned = owned || address.IP.String() == result.Peer
		}
		if !owned || result.Peer == "192.0.2.100" {
			t.Fatal("VIP hairpin did not use a product address", result)
		}
		t.Logf("VIP selected %s; HTTPS backend saw translated source %s", backend, result.Peer)
	}
	checkRejected := func() {
		t.Helper()
		started := time.Now()
		resets, accepted := 0, 0
		// Health publication precedes asynchronous network application. Keep the
		// actual client refusal and its ICMP packet as the completion condition.
		until(t, 8*time.Second, func() bool {
			var page network.DesiredPage
			if err := f.network.Do(t.Context(), "GET", "/v1/observations/desired", nil, &page); err != nil {
				t.Fatal(err)
			}
			var applied network.Desired
			if err := json.Unmarshal(page.Data, &applied); err != nil {
				t.Fatal(err)
			}
			var destinations []string
			if applied.Firewall != nil {
				for _, rule := range applied.Firewall.NAT {
					if rule.Type == "dnat" && rule.Match.Destination == "192.0.2.248/32" {
						destinations = append(destinations, rule.Address)
					}
				}
			}
			after := time.Now()
			result := request("192.0.2.248")
			if result.Reset {
				resets++
			} else if !result.Refused {
				accepted++
			}
			t.Logf("unhealthy transition elapsed=%s applied=%s DNAT=%v client=%+v", time.Since(started), page.Status, destinations, result)
			if !result.Refused || page.Status != "applied" || len(destinations) != 0 {
				return false
			}
			capture.mu.Lock()
			defer capture.mu.Unlock()
			for _, p := range capture.packets {
				b := p.data
				if p.at.Before(after) || len(b) < 38 || binary.BigEndian.Uint16(b[12:14]) != 0x0800 || b[23] != 1 || !bytes.Equal(b[30:34], net.ParseIP("192.0.2.100").To4()) {
					continue
				}
				offset := 14 + int(b[14]&15)*4
				quoted := offset + 8
				if len(b) < quoted+20 || b[offset] != 3 || b[offset+1] != 3 {
					continue
				}
				ports := quoted + int(b[quoted]&15)*4
				if len(b) >= ports+4 && bytes.Equal(b[quoted+16:quoted+20], net.ParseIP("192.0.2.248").To4()) && int(binary.BigEndian.Uint16(b[ports:ports+2])) == result.SourcePort && binary.BigEndian.Uint16(b[ports+2:ports+4]) == 6443 {
					t.Logf("explicit ICMP refusal after %s; transient resets=%d accepted=%d", time.Since(started), resets, accepted)
					return true
				}
			}
			return false
		})
	}
	for _, address := range []string{"192.0.2.62", "192.0.2.63", "192.0.2.64"} {
		waitBackend(address, false)
	}
	waitPriority(90, time.Time{})
	checkRejected()
	// 204 is reachable HTTPS but is not the configured 200 readiness result.
	t.Log("HTTPS 503/204/503: all unready, explicit ICMP rejection, wire priority 90")
	setStatus([3]int{200, 200, 200})
	until(t, 8*time.Second, func() bool { return healthState("192.0.2.62").State.ConsecutivePassed > 0 })
	if s := healthState("192.0.2.62"); s.State.ConsecutivePassed == 1 && s.Active {
		t.Fatal("one success bypassed healthyThreshold=2")
	}
	for _, address := range []string{"192.0.2.62", "192.0.2.63", "192.0.2.64"} {
		waitBackend(address, true)
	}
	waitPriority(150, time.Now())
	checkVIP("cp-01")
	if result := request("192.0.2.62"); result.Backend != "cp-01" || result.Peer != "192.0.2.100" {
		t.Fatal("direct backend traffic was source-translated", result)
	}
	until(t, time.Second, func() bool { return capture.count("192.0.2.100", "192.0.2.62", 6, 6443) > 0 })
	t.Log("direct backend HTTPS traversed the product and retained the client source")
	setStatus([3]int{503, 200, 200})
	until(t, 8*time.Second, func() bool { return healthState("192.0.2.62").State.ConsecutiveFailed > 0 })
	if s := healthState("192.0.2.62"); s.State.ConsecutiveFailed == 1 && !s.Active {
		t.Fatal("one failure bypassed unhealthyThreshold=2")
	}
	waitBackend("192.0.2.62", false)
	checkVIP("cp-02")
	waitPriority(90, time.Now())
	t.Log("one backend unhealthy: forwarding continued through cp-02, VRRP wire priority 90")
	failedAt := time.Now()
	setStatus([3]int{503, 204, 503})
	waitBackend("192.0.2.63", false)
	waitBackend("192.0.2.64", false)
	waitPriority(90, failedAt)
	checkRejected()
	recoveredAt := time.Now()
	setStatus([3]int{200, 200, 200})
	for _, address := range []string{"192.0.2.62", "192.0.2.63", "192.0.2.64"} {
		waitBackend(address, true)
	}
	waitPriority(150, recoveredAt)
	t.Log("all backends recovered: VRRP wire priority restored to 150")
	checkVIP("cp-01")
	// Peer observations prove the custom path, Host, TLS SNI and exact status
	// actually reached independent HTTPS servers, rather than just the config.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]bool{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event struct {
			Backend string `json:"backend"`
			Path    string `json:"path"`
			Host    string `json:"host"`
			SNI     string `json:"sni"`
			Status  int    `json:"status"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal("HTTPS peer output", err, string(line))
		}
		if event.Path == "/readyz" && event.Host == "k8s-api.lain.local" && event.SNI == "k8s-api.lain.local" && event.Status == 200 {
			observed[event.Backend] = true
		}
	}
	if len(observed) != 3 {
		t.Fatal("independent HTTPS readiness observations incomplete", observed)
	}
	t.Log("imported ingress: exact HTTPS readiness, thresholds, ordered backend handoff/recovery, scoped same-LAN hairpin, all-unhealthy rejection and VRRP wire priority 150/90/150 passed")
}

const ingressHTTPSPeer = `import http.server,json,pathlib,ssl,sys,threading
status,cert,key,ready=map(pathlib.Path,sys.argv[1:])
lock=threading.Lock()
class Handler(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_GET(self):
  name,index=self.server.identity
  code=200
  if self.path=='/readyz':
   code=json.loads(status.read_text())[index]
   if self.headers.get('Host')!='k8s-api.lain.local' or getattr(self.connection,'server_name','')!='k8s-api.lain.local':code=421
  elif self.path!='/who':code=404
  payload=b'' if self.path=='/readyz' else json.dumps({'backend':name,'peer':self.client_address[0]}).encode()
  with lock:print(json.dumps({'backend':name,'path':self.path,'host':self.headers.get('Host'),'sni':getattr(self.connection,'server_name',''),'status':code}),flush=True)
  self.send_response(code);self.send_header('Content-Length',str(len(payload)));self.end_headers();self.wfile.write(payload)
servers=[]
for index,address in enumerate(('192.0.2.62','192.0.2.63','192.0.2.64')):
 server=http.server.ThreadingHTTPServer((address,6443),Handler)
 server.identity=('cp-%02d'%(index+1),index)
 context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);context.load_cert_chain(str(cert),str(key))
 context.set_servername_callback(lambda sock,name,ctx:setattr(sock,'server_name',name))
 server.socket=context.wrap_socket(server.socket,server_side=True)
 servers.append(server);threading.Thread(target=server.serve_forever,daemon=True).start()
ready.write_text('ready')
threading.Event().wait()
`

const ingressHTTPSClient = `import http.client,json,socket,ssl,sys
raw=socket.socket();raw.settimeout(2);raw.bind(('0.0.0.0',0))
port=raw.getsockname()[1]
try:
 raw.connect((sys.argv[1],6443))
 context=ssl._create_unverified_context()
 conn=context.wrap_socket(raw,server_hostname='k8s-api.lain.local')
except ConnectionRefusedError:
 print(json.dumps({'refused':True,'source_port':port}));sys.exit(0)
except ConnectionResetError:
 print(json.dumps({'reset':True,'source_port':port}));sys.exit(0)
with conn:
 conn.sendall(b'GET /who HTTP/1.1\r\nHost: k8s-api.lain.local\r\nConnection: close\r\n\r\n')
 response=http.client.HTTPResponse(conn);response.begin()
 assert response.status==200,response.status
 print(response.read().decode())
`
