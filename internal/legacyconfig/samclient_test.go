package legacyconfig

import (
	"testing"
)

func TestSAMClientImportsActualEndpointAndRefreshShape(t *testing.T) {
	source := []byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: lab}
spec:
  resources:
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMEnrollmentClaim
      metadata: {name: leaf}
      spec:
        policyRef: SAMEnrollmentPolicy/leaves
        rrSetRef: SAMRRSet/rrs
        leafID: leaf-a
        tunnelAddress: 10.255.0.2
        endpoint: 192.0.2.2
        directMesh: true
        joinNonce: fixture-nonce
        joinTimestamp: "2026-10-31T00:00:00Z"
        joinHMAC: fixture-signature
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMEnrollmentClient
      metadata: {name: leaf}
      spec:
        claimRef: SAMEnrollmentClaim/leaf
        bootstrapEndpoints: [http://192.0.2.1:65432, http://192.0.2.3:65432]
        controlAPITokenFrom: {file: /etc/routerd/enrollment-token}
        stateTTLRefreshBefore: 10m
        retryBackoff: {min: 10s, max: 15m}
`)
	result, err := Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	got := service(t, result.Document, "sam_client").SAMClient
	if got.ClaimName != "leaf" || !got.Claim.DirectMesh || got.Claim.JoinHMAC != "fixture-signature" || len(got.Endpoints) != 2 || got.TokenFile != "/etc/routerd/enrollment-token" || got.RefreshBeforeSeconds != 600 || got.RetryMinSeconds != 10 || got.RetryMaxSeconds != 900 {
		t.Fatalf("lost enrollment policy: %+v", got)
	}
}
