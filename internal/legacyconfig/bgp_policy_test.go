// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"testing"
)

func TestBGPAdvertisedNextHopFromMigrationPolicy(t *testing.T) {
	input := []byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: shared-gateway}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: BGPRouter
      metadata: {name: edge}
      spec:
        asn: 64500
        routerID: 192.0.2.2
        exportPolicy: {allowedPrefixes: [198.18.0.0/16]}
`)
	options, err := DecodeOptions(bytes.NewBufferString(`schema_version: 1
bgp_next_hops:
  BGPRouter/edge: 192.0.2.254
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := CompileWithOptions(input, options)
	if err != nil {
		t.Fatal(err)
	}
	paths := service(t, result.Document, "bgp").BGP.Runtime.Paths
	if len(paths) != 1 || paths[0].Prefix != "198.18.0.0/16" || paths[0].Attrs.NextHop != "192.0.2.254" {
		t.Fatalf("explicit shared gateway was not retained in BGP advertisements: %+v", paths)
	}
}
