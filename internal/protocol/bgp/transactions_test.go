package bgp

import (
	"context"
	"errors"
	"testing"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	gobgpapi "github.com/osrg/gobgp/v4/api"
)

type journalInspectServer struct {
	*fakePathServer
	statePath string
	onDelete  func(bgpdaemon.AppliedConfig)
}

func (s *journalInspectServer) DeletePath(ctx context.Context, req *gobgpapi.DeletePathRequest) error {
	applied, _, err := readApplied(s.statePath)
	if err != nil {
		return err
	}
	if s.onDelete != nil {
		s.onDelete(applied)
	}
	return s.fakePathServer.DeletePath(ctx, req)
}

func TestDynamicWithdrawalIsDurableBeforeLiveDeleteAndCannotResurrect(t *testing.T) {
	cfg := moduleConfig(t)
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	path := bgpdaemon.AppliedPath{Source: "MobilityPool/demo/node/a", Prefix: "10.99.0.7/32", UUID: "abcd"}
	applied, _, _ := readApplied(cfg.StatePath)
	applied.Paths = append(applied.Paths, path)
	if err := writeApplied(cfg.StatePath, applied); err != nil {
		t.Fatal(err)
	}
	server := &journalInspectServer{fakePathServer: &fakePathServer{deleteErr: errors.New("live engine unavailable")}, statePath: cfg.StatePath, onDelete: func(journal bgpdaemon.AppliedConfig) {
		if len(bgpdaemon.NonStaticPaths(journal.Paths)) != 0 {
			t.Fatal("live delete preceded durable withdrawal fence")
		}
	}}
	_, err := deleteDynamicPath(context.Background(), server, cfg.StatePath, path)
	var fatal *pathTransactionError
	if !errors.As(err, &fatal) {
		t.Fatalf("unsafe live failure did not require worker restart: %v", err)
	}
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	restored, _, _ := readApplied(cfg.StatePath)
	if len(bgpdaemon.NonStaticPaths(restored.Paths)) != 0 {
		t.Fatal("failed live withdrawal resurrected from disk")
	}
}

func TestDynamicReplacementWithdrawsOldJournalBeforeChangingNativePath(t *testing.T) {
	cfg := moduleConfig(t)
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	path := bgpdaemon.AppliedPath{Source: "MobilityPool/demo/node/a", Prefix: "10.99.0.7/32", UUID: "abcd"}
	applied, _, _ := readApplied(cfg.StatePath)
	applied.Paths = append(applied.Paths, path)
	if err := writeApplied(cfg.StatePath, applied); err != nil {
		t.Fatal(err)
	}
	called := false
	server := &journalInspectServer{fakePathServer: &fakePathServer{}, statePath: cfg.StatePath, onDelete: func(journal bgpdaemon.AppliedConfig) {
		called = true
		if len(bgpdaemon.NonStaticPaths(journal.Paths)) != 0 {
			t.Fatal("old path is still in restart set")
		}
	}}
	path.Attrs.LocalPref = 200
	_, updated, err := upsertDynamicPath(context.Background(), server, cfg.StatePath, path)
	if err != nil || !called || updated == nil || updated.Attrs.LocalPref != 200 || updated.UUID == "abcd" {
		t.Fatalf("replace updated=%#v err=%v", updated, err)
	}
}

func TestDeletingLastDynamicPrefixStillRefreshesInboundAndExportPolicy(t *testing.T) {
	server := &fakePathServer{}
	applied := bgpdaemon.AppliedConfig{Peers: map[string]bgpdaemon.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ExportPolicyName: "export", ExportPolicy: bgpdaemon.AppliedExportPolicy{AllowedPrefixes: []string{"192.0.2.0/24"}}}}}
	if err := refreshDynamicPathPolicies(context.Background(), server, applied); err != nil {
		t.Fatal(err)
	}
	if len(server.resetRequests) != 2 || server.resetRequests[0].GetDirection() != gobgpapi.ResetPeerRequest_DIRECTION_IN || server.resetRequests[1].GetDirection() != gobgpapi.ResetPeerRequest_DIRECTION_OUT {
		t.Fatalf("last-prefix reset was omitted: %#v", server.resetRequests)
	}
}
