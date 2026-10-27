// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func TestTailscaleImportReplacesEnvironmentEnrollmentExplicitly(t *testing.T) {
	input, err := os.ReadFile("../integration/testdata/tailscale-import.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../integration/testdata/tailscale-import-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	options, err := DecodeOptions(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	result, err := CompileWithOptions(input, options)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := config.Compile(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	allow, deny := -1, -1
	for i, rule := range plan.Native.Network.Firewall.Input.Rules {
		if rule.Match.InInterface != "uplink0" {
			continue
		}
		if rule.Action == "accept" && rule.Match.Protocol == "udp" {
			for _, port := range rule.Match.DestinationPorts {
				if port.Start == 41641 {
					allow = i
				}
			}
		}
		if rule.Action == "drop" {
			deny = i
		}
	}
	if allow < 0 || deny <= allow {
		t.Fatal("lost legacy Tailscale listener exception before the untrust ACL", allow, deny)
	}
	node := service(t, result.Document, "tailscale").Tailscale
	if node.AuthKeyFile != "/fixture/auth.key" || node.ServerURL != "https://control.test:8443" || !node.AdvertiseExitNode || len(node.AdvertiseRoutes) != 2 {
		t.Fatal("lost declared node intent or selected enrollment", node)
	}
	encoded, err := json.Marshal(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("/discarded/")) || bytes.Contains(encoded, []byte("TS_AUTHKEY")) {
		t.Fatal("imported systemd enrollment state")
	}
	policy := options.Tailscale["TailscaleNode/overlay"]
	policy.Enrollment = "saved_identity"
	policy.AuthKeyFile = ""
	options.Tailscale["TailscaleNode/overlay"] = policy
	saved, err := CompileWithOptions(input, options)
	if err != nil {
		t.Fatal(err)
	}
	if service(t, saved.Document, "tailscale").Tailscale.AuthKeyFile != "" {
		t.Fatal("saved identity retained an enrollment key path")
	}
	for _, bad := range [][]byte{bytes.Replace(input, []byte("acceptDNS: false"), []byte("acceptDNS: true"), 1), bytes.Replace(input, []byte("acceptDNS: false"), []byte("acceptDNS: false\n        ssh: true"), 1)} {
		result, err := CompileWithOptions(bad, options)
		if err == nil || !reflect.DeepEqual(result, Result{}) {
			t.Fatal("unsupported semantics returned partial intent", err)
		}
		if strings.Contains(err.Error(), "/discarded/") {
			t.Fatal("diagnostic leaked source enrollment")
		}
	}
}
