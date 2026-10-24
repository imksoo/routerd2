//go:build linux

package network

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func raWriterDisabled() Interface {
	zero, disabled := 0, false
	return Interface{Name: "wan0", IPv6AcceptRA: &zero, IPv6Autoconf: &disabled}
}

func TestKernelRAWriterOwnershipOrderRestartAndRestore(t *testing.T) {
	k := fake()
	l := k.links["wan0"]
	acceptRA, autoconf := 2, true
	l.IPv6AcceptRA, l.IPv6Autoconf = &acceptRA, &autoconf
	k.links[l.Name] = l
	e, options := engineFor(t, k)
	want := raWriterDisabled()
	want.Addresses = []string{"2001:db8::2/128"}
	d := Desired{Interfaces: []Interface{want}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(k.writes, []string{"setting-accept_ra", "setting-autoconf", "setting-up", "address-add"}) {
		t.Fatalf("kernel writers were not stopped before UP/address creation: %v", k.writes)
	}
	for _, record := range e.state.Settings {
		if record.Index != l.Index || record.Attempted != nil {
			t.Fatalf("setting identity or write intent not committed: %+v", record)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(options, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	k.writes = nil
	if _, err := e.Apply(context.Background(), 1, d); err != nil || len(k.writes) != 0 {
		t.Fatalf("restart changed converged settings: %v, %v", err, k.writes)
	}
	if _, err := e.Apply(context.Background(), 2, Desired{}); err != nil {
		t.Fatal(err)
	}
	l = k.links["wan0"]
	if *l.IPv6AcceptRA != 2 || !*l.IPv6Autoconf || l.Up || len(l.Addresses) != 0 || len(e.state.Settings) != 0 {
		t.Fatalf("original interface state not restored: %+v", l)
	}
	if !reflect.DeepEqual(k.writes, []string{"setting-up", "address-delete", "setting-accept_ra", "setting-autoconf"}) {
		t.Fatalf("kernel RA writers resumed before removing their owned addresses: %v", k.writes)
	}
}

func TestKernelRAWriterDoesNotRestoreReplacementOrForeignDrift(t *testing.T) {
	for _, mode := range []string{"replacement", "drift"} {
		t.Run(mode, func(t *testing.T) {
			k := fake()
			l := k.links["wan0"]
			l.Up = true // UP is already correct, so it remains unmanaged.
			k.links[l.Name] = l
			e, _ := engineFor(t, k)
			d := Desired{Interfaces: []Interface{raWriterDisabled()}}
			if _, err := e.Apply(context.Background(), 1, d); err != nil {
				t.Fatal(err)
			}
			l = k.links["wan0"]
			if mode == "replacement" {
				l.Index = 99
			} else {
				changed := 2
				l.IPv6AcceptRA = &changed
			}
			k.links[l.Name] = l
			k.writes = nil
			if _, err := e.Apply(context.Background(), 2, d); err == nil {
				t.Fatal("adopted a replacement interface or foreign RA setting")
			}
			if _, err := e.Apply(context.Background(), 3, Desired{}); err == nil {
				t.Fatal("restored a replacement interface or foreign RA setting")
			}
			if len(k.writes) != 0 {
				t.Fatalf("unowned state was modified: %v", k.writes)
			}
		})
	}
}

type interruptedSettingKernel struct {
	*fakeKernel
	failures int
}

func (k *interruptedSettingKernel) WriteSetting(key settingKey, value int) error {
	if err := k.fakeKernel.WriteSetting(key, value); err != nil {
		return err
	}
	if key.Kind == "accept_ra" && k.failures > 0 {
		k.failures--
		return &partialMutationError{errors.New("interrupted after the kernel write")}
	}
	return nil
}

func TestKernelRAWriterInterruptedApplyAndRestoreKeepBeforeImage(t *testing.T) {
	k := &interruptedSettingKernel{fakeKernel: fake(), failures: 2}
	opts := Options{StatePath: filepath.Join(t.TempDir(), "ownership.json")}
	for attempt := 0; attempt < 3; attempt++ {
		e, err := newEngine(opts, k)
		if err != nil {
			t.Fatal(err)
		}
		d := Desired{}
		if attempt == 0 {
			d.Interfaces = []Interface{raWriterDisabled()}
		}
		_, err = e.Apply(context.Background(), uint64(attempt), d)
		if attempt < 2 {
			var partial *ApplyError
			if !errors.As(err, &partial) || !partial.Partial {
				t.Fatalf("interrupted write not reported as partial: %v", err)
			}
			if len(e.state.Settings) != 1 || e.state.Settings[0].Before != 1 || e.state.Settings[0].Attempted == nil {
				t.Fatalf("original state or durable write intent lost: %+v", e.state.Settings)
			}
		} else if err != nil || len(e.state.Settings) != 0 {
			t.Fatalf("restart could not finish restoration: %v, %+v", err, e.state.Settings)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
	value, _ := k.ReadSetting(settingKey{Kind: "accept_ra", Interface: "wan0"})
	if value != 1 {
		t.Fatalf("restored RA setting %d, want original 1", value)
	}
}

func TestKernelRASettingValidation(t *testing.T) {
	for _, invalid := range []int{-1, 3} {
		l := raWriterDisabled()
		l.IPv6AcceptRA = &invalid
		if err := Validate(Desired{Interfaces: []Interface{l}}); err == nil {
			t.Fatalf("accepted accept_ra=%d", invalid)
		}
	}
	for _, name := range []string{"all", "default", "../all"} {
		l := raWriterDisabled()
		l.Name = name
		if err := Validate(Desired{Interfaces: []Interface{l}}); err == nil {
			t.Fatalf("accepted reserved sysctl interface %q", name)
		}
		if _, err := settingPath(settingKey{Kind: "accept_ra", Interface: name}); err == nil {
			t.Fatalf("constructed unsafe per-interface path for %q", name)
		}
	}
}
