// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"errors"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
)

func TestSyslogMigrationRetainsDeliveryPolicy(t *testing.T) {
	result := compiled(t, "syslog")
	got := service(t, result.Document, "syslog").Syslog
	want := syslogconfig.Config{Address: "127.0.0.1:5514", Facility: "local6", Tag: "routerd", MinLevel: "info"}
	if *got != want {
		t.Fatalf("routerd syslog defaults changed: %+v", *got)
	}
	input := strings.Replace(string(fixture(t, "syslog")),
		"syslog: {network: udp, address: \"127.0.0.1:5514\"}",
		"minLevel: warning\n        syslog: {network: udp, address: \"[::1]:5515\", facility: daemon, tag: edge}", 1)
	result, err := Compile([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	got = service(t, result.Document, "syslog").Syslog
	want = syslogconfig.Config{Address: "[::1]:5515", Facility: "daemon", Tag: "edge", MinLevel: "warning"}
	if *got != want {
		t.Fatalf("explicit syslog policy changed: %+v", *got)
	}
}

func TestSyslogMigrationDoesNotActivateDisabledSinkOrChangeTransport(t *testing.T) {
	input := string(fixture(t, "syslog"))
	disabled := strings.Replace(input, "type: syslog", "type: syslog\n        enabled: false", 1)
	result, err := Compile([]byte(disabled))
	if err != nil {
		t.Fatal(err)
	}
	if len(nativeProgram(t, result.Document).Services) != 0 || len(result.Mappings) != 1 || result.Mappings[0].Targets[0] != "disabled" {
		t.Fatal("disabled source created a sender or lost its mapping")
	}
	for _, network := range []string{"", "tcp"} {
		source := strings.Replace(input, "network: udp, ", "", 1)
		if network != "" {
			source = strings.Replace(input, "network: udp", "network: "+network, 1)
		}
		result, err := Compile([]byte(source))
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || len(result.Document.Resources) != 0 {
			t.Fatalf("non-UDP source was silently converted: network=%q error=%v", network, err)
		}
	}
}
