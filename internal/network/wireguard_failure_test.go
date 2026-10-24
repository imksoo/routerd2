//go:build linux

package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type faultWireGuardKernel struct {
	*fakeWireGuardKernel
	afterUp  func() error
	readFail bool
}

func (f *faultWireGuardKernel) SetWireGuardUp(index int, up bool) error {
	if err := f.fakeWireGuardKernel.SetWireGuardUp(index, up); err != nil {
		return err
	}
	if up && f.afterUp != nil {
		hook := f.afterUp
		f.afterUp = nil
		return hook()
	}
	return nil
}

func (f *faultWireGuardKernel) ReadWireGuard(index int) (wireGuardDevice, error) {
	if f.readFail {
		f.readFail = false
		return wireGuardDevice{}, errors.New("injected post-UP readback failure")
	}
	return f.fakeWireGuardKernel.ReadWireGuard(index)
}

func TestWireGuardFailureAfterUpClosesOwnedDevice(t *testing.T) {
	for _, stage := range []string{"journal", "canceled readback", "canceled acknowledgment", "lost UP acknowledgment"} {
		t.Run(stage, func(t *testing.T) {
			f := &faultWireGuardKernel{fakeWireGuardKernel: fakeWireGuard()}
			path := filepath.Join(t.TempDir(), "network.json")
			e, err := newEngine(Options{StatePath: path}, f)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			d := fixtureWireGuardDesired(t)
			if _, err := e.Apply(context.Background(), 1, d); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.afterUp = func() error {
				switch stage {
				case "journal":
					// Ordinary private files only: force rename-to-journal failure
					// after the kernel has acknowledged UP.
					if err := os.Rename(path, path+".last"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "canceled readback":
					f.readFail = true
					cancel()
				case "canceled acknowledgment":
					cancel()
				case "lost UP acknowledgment":
					return errors.New("injected lost UP acknowledgment")
				}
				return nil
			}
			d.WireGuard[0].Peers[0].PersistentKeepalive++
			if _, err := e.Apply(ctx, 2, d); err == nil {
				t.Fatal("post-UP failure was acknowledged")
			}
			if f.links["wg0"].Up {
				t.Fatal("failed replacement left its owned device UP")
			}
			if record := e.wireGuardRecord("wg0"); record == nil || !record.Quarantined || record.Pending == nil {
				t.Fatal("failed replacement lost its recovery intent")
			}
		})
	}
}

func TestWireGuardInterruptedEndpointChangeRollsBackDeclaration(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		name := "one interruption"
		if repeat {
			name = "interrupted rollback"
		}
		t.Run(name, func(t *testing.T) { testWireGuardEndpointRollback(t, repeat) })
	}
}

func testWireGuardEndpointRollback(t *testing.T, interruptRollback bool) {
	f := fakeWireGuard()
	path := filepath.Join(t.TempDir(), "network.json")
	e, err := newEngine(Options{StatePath: path}, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	d := fixtureWireGuardDesired(t)
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	changed := cloneDesired(d)
	changed.WireGuard[0].Peers[0].Endpoint = "192.0.2.200:60000"
	f.failAfterPeer = 1
	if _, err := e.Apply(context.Background(), 2, changed); err == nil {
		t.Fatal("interrupted endpoint change succeeded")
	}
	if interruptRollback {
		// Fail after writing a second recovery intent but before its SET. The
		// live candidate endpoint must not become "roaming" merely because the
		// newly recorded pending declaration is now the old committed one.
		f.failAfterPeer = -1
		f.fail = "wireguard-replace"
		if _, err := e.Apply(context.Background(), 1, d); err == nil {
			t.Fatal("interrupted rollback succeeded")
		}
		f.fail = ""
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(Options{StatePath: path}, f)
	if err != nil {
		t.Fatal(err)
	}
	f.failAfterPeer = -1
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	live := f.devices[f.links["wg0"].Index]
	if live.Peers[0].Endpoint != d.WireGuard[0].Peers[0].Endpoint {
		t.Fatalf("uncommitted endpoint survived rollback as roaming: %s", live.Peers[0].Endpoint)
	}
}

func TestWireGuardQuarantinePreservesForeignState(t *testing.T) {
	for _, changed := range []string{"link owner", "foreign peer"} {
		t.Run(changed, func(t *testing.T) {
			f := &faultWireGuardKernel{fakeWireGuardKernel: fakeWireGuard()}
			e, err := newEngine(Options{StatePath: filepath.Join(t.TempDir(), "network.json")}, f)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			d := fixtureWireGuardDesired(t)
			if _, err := e.Apply(context.Background(), 1, d); err != nil {
				t.Fatal(err)
			}
			f.afterUp = func() error {
				link := f.links["wg0"]
				if changed == "link owner" {
					link.Alias = "another-owner"
					f.links["wg0"] = link
				} else {
					device := f.devices[link.Index]
					key, _ := fixtureWireGuardKey(90).public()
					device.Peers = append(device.Peers, wireGuardNativePeer{PublicKey: key, AllowedIPs: []string{"203.0.113.0/24"}})
					f.devices[link.Index] = device
				}
				return errors.New("injected ownership change after UP")
			}
			d.WireGuard[0].Peers[0].PersistentKeepalive++
			if _, err := e.Apply(context.Background(), 2, d); err == nil {
				t.Fatal("foreign mutation was acknowledged")
			}
			if !f.links["wg0"].Up || f.writes[len(f.writes)-1] != "wireguard-up-true" {
				t.Fatal("quarantine changed a device whose ownership was lost")
			}
		})
	}
}
