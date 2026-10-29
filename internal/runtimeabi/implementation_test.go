//go:build linux

package runtimeabi_test

import (
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/management"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/runtimeabi"
)

// Keep metadata-only production dependencies small, but test that explicit
// persisted format implementations cannot change without updating their claim.
func TestDeclarationTracksImplementedFormatVersions(t *testing.T) {
	d := runtimeabi.Current()
	if d.Configuration.Write != uint32(config.SchemaVersion) {
		t.Fatal("configuration schema changed without an ABI declaration")
	}
	formats := map[string]uint32{"configuration": config.JournalVersion, "events": events.SchemaVersion, "network": network.JournalVersion, "release": release.StateVersion, "snapshot": config.SnapshotVersion, "ssh_identity": management.IdentityVersion}
	if len(d.State) != len(formats) {
		t.Fatal("current ABI omitted a persistent component")
	}
	for _, s := range d.State {
		if expected, ok := formats[s.Name]; !ok || s.Format.Write != expected {
			t.Fatalf("%s implementation=%d declaration=%d", s.Name, expected, s.Format.Write)
		}
	}
}

func TestConditionalNetworkLifetimesRequireAnExplicitCompilerContract(t *testing.T) {
	current := runtimeabi.Current()
	if current.ConfigurationContract != "resource-program-native-62" {
		t.Fatal("Conditional network lifetime semantics require their signed compiler contract")
	}
	facts := runtimeabi.Facts{Configuration: current.Configuration.Write, ConfigurationContract: current.ConfigurationContract}
	for _, state := range current.State {
		facts.State = append(facts.State, runtimeabi.StateVersion{Name: state.Name, Version: state.Format.Write})
	}
	if err := runtimeabi.CheckTransition(current, current, facts); err != nil {
		t.Fatal(err)
	}
	previous := current
	previous.ConfigurationContract = "resource-program-native-28"
	facts.ConfigurationContract = previous.ConfigurationContract
	if err := runtimeabi.CheckTransition(previous, current, facts); err == nil {
		t.Fatal("unqualified prototype compiler transition accepted")
	}
}
