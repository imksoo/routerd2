package serviceconfig

import (
	"encoding/json"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
)

func TestSyslogInstancesHaveIndependentDestinations(t *testing.T) {
	services := []Service{
		{Name: "logs-v4", Syslog: &syslogconfig.Config{Address: "192.0.2.10:514", Facility: "local0", Tag: "routerd", MinLevel: "info"}},
		{Name: "logs-v6", Syslog: &syslogconfig.Config{Address: "[2001:db8::10]:514", Facility: "local1", Tag: "edge", MinLevel: "warning"}},
	}
	if err := Validate(services); err != nil {
		t.Fatal(err)
	}
	for _, service := range services {
		if service.Interface() != "" {
			t.Fatal("syslog forwarding must not require an interface")
		}
		instance, err := service.Instance("/run/routerd", "/var/lib/routerd")
		if err != nil {
			t.Fatal(err)
		}
		if instance.Role != "syslog" || instance.Name != "service."+service.Name {
			t.Fatalf("wrong worker identity: %+v", instance)
		}
		var cfg syslogconfig.Config
		if err := json.Unmarshal(instance.Config, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg != *service.Syslog {
			t.Fatalf("destination changed during worker compilation: got %+v, want %+v", cfg, *service.Syslog)
		}
	}
}
