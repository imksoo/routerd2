//go:build linux && integration

// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestIsolatedNativeAFTR(t *testing.T) {
	if os.Getenv("ROUTERD2_AFTR_CHILD") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		parent := os.Getenv("ROUTERD2_AFTR_PARENT")
		if err != nil || parent == "" || current == parent {
			t.Fatal("refusing AFTR fixture outside a fresh network namespace")
		}
		nativeAFTR(t)
		return
	}
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestIsolatedNativeAFTR$", "-test.v", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), "ROUTERD2_AFTR_CHILD=1", "ROUTERD2_AFTR_PARENT="+parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	out, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
		if errors.Is(err, syscall.ENOENT) {
			unavailable = exec.Command(exe, "-test.run=^$").Run() == nil
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated AFTR namespace unavailable: %v", err)
		}
		t.Fatalf("native AFTR fixture: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func inAFTRNamespace(ns netns.NsHandle, fn func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	prior, err := netns.Get()
	if err != nil {
		return err
	}
	defer prior.Close()
	if err = netns.Set(ns); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, netns.Set(prior)) }()
	return fn()
}

func aftrServer(t *testing.T) (Resolver, *atomic.Int32, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	runtime.LockOSThread()
	prior, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	peerNS, createErr := netns.New()
	restoreErr := netns.Set(prior)
	prior.Close()
	runtime.UnlockOSThread()
	if err = errors.Join(createErr, restoreErr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peerNS.Close() })
	if err = netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "awan"}, PeerName: "adns"}); err != nil {
		t.Fatal(err)
	}
	client, err := netlink.LinkByName("awan")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := netlink.LinkByName("adns")
	if err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetNsFd(peer, int(peerNS)); err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	peer, err = h.LinkByName("adns")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"fe80::1/64", "2001:db8:1::1/64"} {
		a, _ := netlink.ParseAddr(value)
		a.Flags = unix.IFA_F_NODAD
		if err = netlink.AddrAdd(client, a); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := netlink.ParseAddr("fe80::53/64")
	a.Flags = unix.IFA_F_NODAD
	if err = h.AddrAdd(peer, a); err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(client); err != nil {
		t.Fatal(err)
	}
	if err = h.LinkSetUp(peer); err != nil {
		t.Fatal(err)
	}
	var tcp net.Listener
	var udp, spoof net.PacketConn
	err = inAFTRNamespace(peerNS, func() error {
		lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
			var inner error
			err := raw.Control(func(fd uintptr) {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "adns")
			})
			return errors.Join(err, inner)
		}}
		var err error
		tcp, err = lc.Listen(context.Background(), "tcp6", "[fe80::53%adns]:0")
		if err != nil {
			return err
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		udp, err = lc.ListenPacket(context.Background(), "udp6", fmt.Sprintf("[fe80::53%%adns]:%d", port))
		if err != nil {
			return err
		}
		spoof, err = lc.ListenPacket(context.Background(), "udp6", "[fe80::53%adns]:0")
		return err
	})
	if tcp != nil {
		t.Cleanup(func() { tcp.Close() })
	}
	if udp != nil {
		t.Cleanup(func() { udp.Close() })
	}
	if spoof != nil {
		t.Cleanup(func() { spoof.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	mode, udpQueries, tcpQueries := new(atomic.Int32), new(atomic.Int32), new(atomic.Int32)
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		_, tcpMode := w.RemoteAddr().(*net.TCPAddr)
		if tcpMode {
			tcpQueries.Add(1)
		} else {
			udpQueries.Add(1)
		}
		m := new(dns.Msg)
		m.SetReply(request)
		if mode.Load() == 4 {
			return
		}
		if mode.Load() == 1 {
			m.Question[0].Name = "forged.example."
		}
		if mode.Load() == 2 {
			m.Id++
		}
		if mode.Load() == 0 && !tcpMode {
			m.Truncated = true
			_ = w.WriteMsg(m)
			return
		}
		m.Answer = []dns.RR{
			&dns.CNAME{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 4}, Target: "target.example."},
			&dns.AAAA{Hdr: dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 6}, AAAA: net.ParseIP("2001:db8:ffff::2")},
			&dns.AAAA{Hdr: dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 6}, AAAA: net.ParseIP("2001:db8:ffff::1")},
		}
		if mode.Load() == 3 && !tcpMode {
			b, _ := m.Pack()
			_, _ = spoof.WriteTo(b, w.RemoteAddr())
			return
		}
		_ = w.WriteMsg(m)
	})
	for _, server := range []*dns.Server{{Net: "udp", PacketConn: udp, Handler: handler}, {Net: "tcp", Listener: tcp, Handler: handler}} {
		ready, done := make(chan struct{}), make(chan error, 1)
		server.NotifyStartedFunc = func() { close(ready) }
		go func() { done <- server.ActivateAndServe() }()
		t.Cleanup(func() {
			_ = server.Shutdown()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("DNS fixture did not stop")
			}
		})
		select {
		case <-ready:
		case err := <-done:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("DNS fixture startup timeout")
		}
	}
	return Resolver{Address: "fe80::53", Port: uint16(tcp.Addr().(*net.TCPAddr).Port), Interface: "awan"}, mode, udpQueries, tcpQueries
}

func nativeAFTR(t *testing.T) {
	server, mode, udpQueries, tcpQueries := aftrServer(t)
	cfg := configForTest()
	cfg.Interface = "awan"
	cfg.TimeoutSeconds = 1
	cfg.RetrySeconds = 1
	for _, tc := range []struct {
		mode     int32
		contains string
	}{{1, "question mismatch"}, {2, "timeout"}, {3, "timeout"}} {
		mode.Store(tc.mode)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, err := lookupAAAA(ctx, cfg, "2001:db8:1::1", server, "aftr.example")
		cancel()
		var timeout net.Error
		matches := err != nil && strings.Contains(err.Error(), tc.contains)
		if tc.contains == "timeout" {
			matches = errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()
		}
		if !matches {
			t.Fatalf("DNS identity/source mode %d: %v", tc.mode, err)
		}
	}
	mode.Store(0)
	in := liveInput(t)
	in.Interface = "awan"
	in.Resolvers = []Resolver{server}
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	in.Source.PreferredUntil = lifetime.After(clock, 15)
	in.Source.ValidUntil = lifetime.After(clock, 20)
	updates := make(chan Input, 4)
	snapshots := make(chan Snapshot, 64)
	done, finished := make(chan error, 1), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(finished)
		done <- Run(ctx, cfg, in, updates, EffectsFunc(func(_ context.Context, s Snapshot) error { snapshots <- s; return nil }), nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("AFTR worker did not clean up")
		}
	})
	inputs := map[uint64]Input{1: in}
	wait := func(predicate func(Snapshot) bool) Snapshot {
		t.Helper()
		timer := time.NewTimer(7 * time.Second)
		defer timer.Stop()
		for {
			select {
			case s := <-snapshots:
				if err := ValidateSnapshot(cfg, inputs[s.Generation], s); err != nil {
					t.Fatal(err)
				}
				if predicate(s) {
					return s
				}
			case err := <-done:
				t.Fatal(err)
			case <-timer.C:
				t.Fatal("native AFTR snapshot timeout")
			}
		}
	}
	initial := wait(func(s Snapshot) bool { return s.Active })
	if initial.Available {
		t.Fatal("ready invented an unresolved endpoint")
	}
	first := wait(func(s Snapshot) bool { return s.Available })
	if first.Remote != "2001:db8:ffff::2" || first.ResolverIndex != 0 || len(first.Answers) != 2 || first.ValidUntil.Nanos-first.Answers[0].ReceivedNanos > uint64(4*time.Second) || udpQueries.Load() < 1 || tcpQueries.Load() < 1 {
		t.Fatalf("native UDP/TCP/CNAME selection failed: %+v", first)
	}
	mode.Store(4)
	failed := wait(func(s Snapshot) bool { return s.Failures > 0 })
	if !failed.Available || failed.LastError == "" || failed.ValidUntil != first.ValidUntil {
		t.Fatal("transient DNS failure lost or extended the bounded last-good endpoint")
	}
	wait(func(s Snapshot) bool { return !s.Available && s.Failures > 0 })
	next := cloneInput(in)
	next.Generation = 2
	next.UpdatedAt = time.Now().UTC()
	next.Candidates = []Candidate{{Value: "2001:db8:ffff::3", Origin: "configured"}}
	next.Resolvers = nil
	clock, err = boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	next.Source.PreferredUntil = lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds) + uint64(500*time.Millisecond)}
	inputs[2] = next
	updates <- next
	literal := wait(func(s Snapshot) bool { return s.Generation == 2 && s.Available })
	if literal.Remote != "2001:db8:ffff::3" || literal.ResolverIndex != -1 || len(literal.Answers) != 0 {
		t.Fatal("literal fallback still depended on DNS")
	}
	wait(func(s Snapshot) bool { return s.Generation == 2 && !s.Available })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native AFTR cancellation failed")
	}
	withdrawn := false
	for len(snapshots) > 0 {
		s := <-snapshots
		if !s.Active && !s.Available && s.Generation == 2 {
			withdrawn = true
		}
	}
	if !withdrawn {
		t.Fatal("native AFTR worker did not withdraw on shutdown")
	}
}
