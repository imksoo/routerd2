package legacyconfig

import (
	"testing"
	"time"
)

func TestEventGroupResolvesStaticPeersWithoutChangingTheirMeaning(t *testing.T) {
	data := []byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: events}
spec:
  resources:
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMNodeSet
      metadata: {name: group-nodes}
      spec:
        nodes:
          - {nodeRef: local, eventEndpoint: 'http://127.0.0.1:8787'}
          - {nodeRef: remote, eventEndpoint: 'http://192.0.2.2:8787'}
    - apiVersion: federation.routerd.net/v1alpha1
      kind: EventGroup
      metadata: {name: edge}
      spec:
        nodeName: local
        peersFrom: [{resource: SAMNodeSet/group-nodes}]
        auth: {mode: hmac, secretFile: /etc/routerd/event-key}
        listen: {address: 127.0.0.1, port: 8787}
        retention: {maxEvents: 1000, maxAge: 24h}
        replayWindow: 5m
    - apiVersion: federation.routerd.net/v1alpha1
      kind: EventPeer
      metadata: {name: override}
      spec: {groupRef: edge, nodeName: remote, endpoint: 'http://192.0.2.3:8787', types: [fixture.observed]}
`)
	result, err := Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := service(t, result.Document, "event_group").EventGroup
	if len(cfg.Peers) != 1 || cfg.Peers[0].Endpoint != "http://192.0.2.3:8787" || len(cfg.Peers[0].Types) != 1 || cfg.MaxEvents != 1000 || cfg.MaxAge != 24*time.Hour || cfg.ReplayWindow != 5*time.Minute {
		t.Fatal("static peer override or configured retention changed", cfg)
	}
}
