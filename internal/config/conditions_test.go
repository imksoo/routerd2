package config

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// The reused DNS engine exposes source fields that its worker cannot evaluate.
// A gate must never be accepted and then discarded during native compilation.
func TestUnsupportedDNSConditionsCannotStartAnUnconditionalWorker(t *testing.T) {
	base := func() Native {
		return Native{Services: []serviceconfig.Service{{Name: "dns", DNS: &dnsconfig.RuntimeConfig{
			Spec: api.DNSResolverSpec{
				Listen:  []api.DNSResolverListenSpec{{Addresses: []string{"127.0.0.1"}, Port: 5353}},
				Sources: []api.DNSResolverSourceSpec{{Name: "default", Kind: "forward", Match: []string{"."}, Upstreams: []string{"udp://192.0.2.53:53"}}},
			},
		}}}}
	}
	if err := Validate(FromNative(base())); err != nil {
		t.Fatal("unconditional DNS fixture is invalid", err)
	}
	condition := api.ResourceWhenSpec{State: map[string]api.StateMatchSpec{"Health/missing.phase": {Equals: "Ready"}}}
	for _, when := range []api.ResourceWhenSpec{
		condition,
		{All: []api.ResourceWhenSpec{condition}},
		{Any: []api.ResourceWhenSpec{condition}},
		{All: []api.ResourceWhenSpec{{}}},
	} {
		native := base()
		native.Services[0].DNS.Spec.When = when
		if err := Validate(FromNative(native)); err == nil || !strings.Contains(err.Error(), "when conditions") {
			t.Fatal("unevaluated DNS condition was admitted", err)
		}
		encoded, err := YAML(FromNative(native))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(encoded); err == nil || !strings.Contains(err.Error(), "when conditions") {
			t.Fatal("serialized gate was silently normalized away", err)
		}
	}
}
