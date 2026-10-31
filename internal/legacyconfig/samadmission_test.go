package legacyconfig

import "testing"

func TestSAMAdmissionResolvesPolicyAndRRTopologyAtImport(t *testing.T) {
	source := []byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: rr}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: lan}
      spec: {ifname: eth0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DHCPv4Client
      metadata: {name: endpoint}
      spec: {interface: lan, useRoutes: false, useDNS: false}
    - apiVersion: net.routerd.net/v1alpha1
      kind: BGPRouter
      metadata: {name: fabric}
      spec: {asn: 64512, routerID: 192.0.2.1}
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMNodeSet
      metadata: {name: rrs}
      spec: {nodes: [{nodeRef: rr-a, routeReflector: true, samEndpoint: 192.0.2.1}]}
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMTransportProfile
      metadata: {name: rr-a}
      spec:
        selfNodeRef: rr-a
        mode: ipip
        addressingMode: pair-stable
        innerPrefix: 10.255.0.0/24
        underlayInterface: lan
        localEndpointFrom: {resource: DHCPv4Client/endpoint, field: currentAddress}
        peersFrom: [{resource: SAMNodeSet/rrs}]
        bgp: {routerRef: BGPRouter/fabric, peerASN: 64512}
    - apiVersion: system.routerd.net/v1alpha1
      kind: ControlAPI
      metadata: {name: enrollment}
      spec:
        listenAddress: 127.0.0.1
        port: 16543
        allowCIDRs: [127.0.0.1/32]
        tokenFrom: {file: /etc/routerd/control-token}
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMEnrollmentPolicy
      metadata: {name: leaves}
      spec:
        transportProfileRef: SAMTransportProfile/rr-a
        rrSetRef: SAMRRSet/rrs
        rrNodeSetRef: SAMNodeSet/rrs
        joinTokenFrom: {file: /etc/routerd/join-token}
        joinAudience: fixture
        allowedLeafIDs: {pattern: ^leaf-}
        tunnelAddressPrefixes: [10.255.0.0/24]
        endpointPrefixes: [192.0.2.0/24]
        mobilityPrefixes: [198.51.100.0/24]
        directMesh: {peerGroupRef: SAMPeerGroup/direct}
        ttl: 24h
`)
	result, err := Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	got := service(t, result.Document, "sam_admission").SAMAdmission
	if got.Listen.String() != "127.0.0.1:16543" || len(got.Policies) != 1 || got.Policies[0].RRSet.Nodes[0].NodeRef != "rr-a" || got.Policies[0].Spec.TTL != "24h" || got.Policies[0].Spec.JoinTokenFrom.File != "/etc/routerd/join-token" || got.Policies[0].TransportFingerprint == "" {
		t.Fatal("lost admission policy or resolved transport topology")
	}
}
