// SPDX-License-Identifier: BSD-3-Clause
// Adapted from imksoo/routerd at 1329ef54f296507eda79bbbc3b853783dd27251e.
// See NOTICE for the retained license and source attribution.
package bgp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	gobgpapi "github.com/osrg/gobgp/v4/api"
	gobgpapiutil "github.com/osrg/gobgp/v4/pkg/apiutil"
	gobgp "github.com/osrg/gobgp/v4/pkg/packet/bgp"
	gobgpserver "github.com/osrg/gobgp/v4/pkg/server"
)

type pathServer interface {
	AddPath(context.Context, *gobgpapi.AddPathRequest) (*gobgpapi.AddPathResponse, error)
	DeletePath(context.Context, *gobgpapi.DeletePathRequest) error
}

type policyPathServer interface {
	pathServer
	SetPolicies(context.Context, *gobgpapi.SetPoliciesRequest) error
	SetPolicyAssignment(context.Context, *gobgpapi.SetPolicyAssignmentRequest) error
	ResetPeer(context.Context, *gobgpapi.ResetPeerRequest) error
}

// observedPathServer is intentionally separate from pathServer.  The control
// API's /v1/paths endpoint is a serialized read-modify-write view of
// applied.json; a live RIB observation must never be mistaken for that local
// desired-path journal.
type observedPathServer interface {
	ListObservedPaths(context.Context) ([]bgpdaemon.ObservedPath, error)
}

// localPathServer keeps routerd's narrow protobuf-shaped path interface while
// adapting GoBGP v4's in-process native path API. The remote controller uses
// the same protobuf requests over gRPC.
type localPathServer struct {
	*gobgpserver.BgpServer
}

func (s *localPathServer) AddPath(_ context.Context, req *gobgpapi.AddPathRequest) (*gobgpapi.AddPathResponse, error) {
	path, err := nativePath(req.GetPath())
	if err != nil {
		return nil, err
	}
	results, err := s.BgpServer.AddPath(gobgpapiutil.AddPathRequest{
		VRFID: req.GetVrfId(),
		Paths: []*gobgpapiutil.Path{path},
	})
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("GoBGP AddPath returned %d results, want 1", len(results))
	}
	id, err := results[0].UUID.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return &gobgpapi.AddPathResponse{Uuid: id}, results[0].Error
}

func (s *localPathServer) DeletePath(_ context.Context, req *gobgpapi.DeletePathRequest) error {
	request := gobgpapiutil.DeletePathRequest{VRFID: req.GetVrfId()}
	switch {
	case len(req.GetUuid()) > 0:
		id, err := uuid.FromBytes(req.GetUuid())
		if err != nil {
			return err
		}
		request.UUIDs = []uuid.UUID{id}
	case req.GetPath() != nil:
		path, err := nativePath(req.GetPath())
		if err != nil {
			return err
		}
		request.Paths = []*gobgpapiutil.Path{path}
	default:
		request.DeleteAll = true
		if family := req.GetFamily(); family != nil {
			nativeFamily := gobgp.NewFamily(uint16(family.GetAfi()), uint8(family.GetSafi()))
			request.DeleteFamily = &nativeFamily
		}
	}
	return s.BgpServer.DeletePath(request)
}

// ListObservedPaths returns the live IPv4 unicast global RIB.  The response
// keeps only the fields needed by routerd's control-plane safety checks; it is
// never persisted and is not an alternate configuration API.
func (s *localPathServer) ListObservedPaths(ctx context.Context) ([]bgpdaemon.ObservedPath, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []bgpdaemon.ObservedPath
	var callbackErr error
	err := s.BgpServer.ListPath(gobgpapiutil.ListPathRequest{
		TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL,
		Family:    gobgp.RF_IPv4_UC,
	}, func(prefix gobgp.NLRI, paths []*gobgpapiutil.Path) {
		if callbackErr != nil || ctx.Err() != nil {
			callbackErr = ctx.Err()
			return
		}
		for _, path := range paths {
			if path == nil || path.Withdrawal {
				continue
			}
			out = append(out, observedPath(prefix.String(), path))
		}
	})
	if err != nil {
		return nil, err
	}
	if callbackErr != nil {
		return nil, callbackErr
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Prefix != out[j].Prefix {
			return out[i].Prefix < out[j].Prefix
		}
		if out[i].PeerAddress != out[j].PeerAddress {
			return out[i].PeerAddress < out[j].PeerAddress
		}
		return out[i].Best && !out[j].Best
	})
	return out, nil
}

func observedPath(prefix string, path *gobgpapiutil.Path) bgpdaemon.ObservedPath {
	out := bgpdaemon.ObservedPath{
		Prefix:      strings.TrimSpace(prefix),
		Best:        path.Best,
		Valid:       !path.IsNexthopInvalid,
		Stale:       path.Stale,
		PeerAddress: path.PeerAddress.String(),
	}
	if !path.PeerAddress.IsValid() {
		out.PeerAddress = ""
	}
	for _, attr := range path.Attrs {
		switch value := attr.(type) {
		case *gobgp.PathAttributeCommunities:
			for _, community := range value.Value {
				out.Communities = append(out.Communities, fmt.Sprintf("%d:%d", community>>16, community&0xffff))
			}
		}
	}
	out.Communities = uniqueTrimmedSorted(out.Communities)
	return out
}

func nativePath(path *gobgpapi.Path) (*gobgpapiutil.Path, error) {
	if path == nil {
		return nil, fmt.Errorf("path is required")
	}
	nlri, err := gobgpapiutil.GetNativeNlri(path)
	if err != nil {
		return nil, err
	}
	attrs, err := gobgpapiutil.GetNativePathAttributes(path)
	if err != nil {
		return nil, err
	}
	family := path.GetFamily()
	if family == nil {
		return nil, fmt.Errorf("path family is required")
	}
	peerID, _ := netip.ParseAddr(path.GetSourceId())
	peerAddress, _ := netip.ParseAddr(path.GetNeighborIp())
	out := &gobgpapiutil.Path{
		Family:             gobgp.NewFamily(uint16(family.GetAfi()), uint8(family.GetSafi())),
		Nlri:               nlri,
		Attrs:              attrs,
		Age:                path.GetAge().GetSeconds(),
		Best:               path.GetBest(),
		Withdrawal:         path.GetIsWithdraw(),
		Stale:              path.GetStale(),
		PeerASN:            path.GetSourceAsn(),
		PeerID:             peerID,
		PeerAddress:        peerAddress,
		IsFromExternal:     path.GetIsFromExternal(),
		NoImplicitWithdraw: path.GetNoImplicitWithdraw(),
		IsNexthopInvalid:   path.GetIsNexthopInvalid(),
		RemoteID:           path.GetIdentifier(),
		LocalID:            path.GetLocalIdentifier(),
	}
	if out.PeerASN != 0 && !out.PeerID.IsValid() {
		return nil, fmt.Errorf("source ID must be set when source ASN is nonzero")
	}
	return out, nil
}

func restoreApplied(ctx context.Context, server *gobgpserver.BgpServer, paths policyPathServer, cfg Config, logger *slog.Logger) error {
	statePath := cfg.StatePath
	applied, ok, err := readApplied(statePath)
	if err != nil {
		return fmt.Errorf("read applied BGP state: %w", err)
	}
	if !ok {
		return nil
	}
	if err := bgpdaemon.Validate(applied); err != nil {
		return fmt.Errorf("validate applied BGP state: %w", err)
	}
	if err := bgpconfig.ValidateBFDReferences(Config{Runtime: applied, PeerBFD: cfg.PeerBFD}); err != nil {
		return fmt.Errorf("persisted BGP BFD intent is incompatible: %w", err)
	}
	global := appliedGlobal(applied.Global)
	global.BindToDevice = cfg.BindInterface
	if err := server.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: global}); err != nil {
		return fmt.Errorf("restore BGP global: %w", err)
	}
	if err := applyAppliedPolicies(ctx, paths, applied); err != nil {
		return fmt.Errorf("restore BGP policy: %w", err)
	}
	withheldDirectPeers := pendingDirectPeerTransitions(applied)
	for _, peer := range sortedPeers(applied.Peers) {
		if withheldDirectPeers[peer.Address] {
			logger.Warn("skip pending direct peer transition during BGP restore", "peer", peer.Address)
			continue
		}
		transport := cfg.PeerTransports[peer.Address]
		if transport.BindInterface == "" {
			transport.BindInterface = cfg.BindInterface
		}
		nativePeer := configuredPeer(peer, applied.Global, transport)
		if _, gated := cfg.PeerBFD[peer.Address]; gated {
			// The FSM is born administratively down. Disabling after AddPeer
			// would leave a window for an initial TCP OPEN or advertisement.
			nativePeer.Conf.AdminDown = true
		}
		if err := server.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: nativePeer}); err != nil {
			return fmt.Errorf("restore BGP peer %s: %w", peer.Address, err)
		}
	}
	if err := restoreAppliedPaths(ctx, paths, &applied); err != nil {
		return err
	}
	if err := refreshDynamicPathPolicies(ctx, paths, applied); err != nil {
		return fmt.Errorf("restore BGP dynamic policy refresh: %w", err)
	}
	if err := writeApplied(statePath, applied); err != nil {
		return fmt.Errorf("persist restored BGP path UUIDs: %w", err)
	}
	logger.Info("restored applied BGP state", "peers", len(applied.Peers), "paths", len(applied.Paths), "advertisements", len(applied.Advertisements), "hash", bgpdaemon.Hash(applied))
	return nil
}

func pendingDirectPeerTransitions(applied bgpdaemon.AppliedConfig) map[string]bool {
	out := map[string]bool{}
	// During an import-policy replacement the persisted peer definitions still
	// describe the pre-change allowlist. Do not revive a direct high-preference
	// session from that snapshot; routerd will reinstall the desired policy,
	// reset inbound state, and then add it again. RR remains the safe path while
	// that transaction is unfinished.
	if applied.PendingImportPolicyReset {
		for address, peer := range applied.Peers {
			if peer.PreserveImportPrefixes {
				out[address] = true
			}
		}
	}
	for _, address := range applied.PendingDirectPeerAdditions {
		if address = strings.TrimSpace(address); address != "" {
			out[address] = true
		}
	}
	for _, address := range applied.PendingDirectPeerRemovals {
		if address = strings.TrimSpace(address); address != "" {
			out[address] = true
		}
	}
	return out
}

func applyAppliedPolicies(ctx context.Context, server policyPathServer, applied bgpdaemon.AppliedConfig) error {
	policies, assignment := appliedPolicies(applied)
	if len(assignment.GetPolicies()) == 0 {
		if err := server.SetPolicyAssignment(ctx, &gobgpapi.SetPolicyAssignmentRequest{Assignment: assignment}); err != nil {
			return fmt.Errorf("apply BGP import policy assignment: %w", err)
		}
	}
	if err := server.SetPolicies(ctx, policies); err != nil {
		return fmt.Errorf("apply BGP policies: %w", err)
	}
	if len(assignment.GetPolicies()) > 0 {
		if err := server.SetPolicyAssignment(ctx, &gobgpapi.SetPolicyAssignmentRequest{Assignment: assignment}); err != nil {
			return fmt.Errorf("apply BGP import policy assignment: %w", err)
		}
	}
	return nil
}

func restoreAppliedPaths(ctx context.Context, server pathServer, applied *bgpdaemon.AppliedConfig) error {
	if applied == nil {
		return nil
	}
	normalized := bgpdaemon.Normalize(*applied)
	for i, appliedPath := range normalized.Paths {
		path, err := pathFromAppliedPath(appliedPath)
		if err != nil {
			return fmt.Errorf("restore BGP path %s/%s: %w", appliedPath.Source, appliedPath.Prefix, err)
		}
		resp, err := server.AddPath(ctx, &gobgpapi.AddPathRequest{TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL, Path: path})
		if err != nil {
			return fmt.Errorf("restore BGP path %s/%s: %w", appliedPath.Source, appliedPath.Prefix, err)
		}
		normalized.Paths[i].UUID = bgpdaemon.EncodeUUID(resp.GetUuid())
	}
	*applied = bgpdaemon.Normalize(normalized)
	return nil
}

func appliedGlobal(global bgpdaemon.AppliedGlobal) *gobgpapi.Global {
	out := &gobgpapi.Global{
		Asn:              global.ASN,
		RouterId:         global.RouterID,
		ListenPort:       int32(global.ListenPort),
		ListenAddresses:  global.ListenAddresses,
		UseMultiplePaths: global.UseMultiplePaths,
	}
	for _, family := range global.Families {
		switch family {
		case "ipv6-unicast":
			out.Families = append(out.Families, 1)
		default:
			out.Families = append(out.Families, 0)
		}
	}
	if len(out.Families) == 0 {
		out.Families = []uint32{0}
	}
	if gr := global.GracefulRestart; gr != nil && gr.Enabled {
		out.GracefulRestart = &gobgpapi.GracefulRestart{Enabled: true, RestartTime: gr.RestartTime, StaleRoutesTime: gr.StaleRoutesTime}
	}
	return out
}

func appliedPeer(peer bgpdaemon.AppliedPeer, global bgpdaemon.AppliedGlobal) *gobgpapi.Peer {
	peerType := gobgpapi.PeerType_PEER_TYPE_EXTERNAL
	if global.ASN != 0 && peer.ASN == global.ASN {
		peerType = gobgpapi.PeerType_PEER_TYPE_INTERNAL
	}
	out := &gobgpapi.Peer{
		Conf: &gobgpapi.PeerConf{
			NeighborAddress: peer.Address,
			PeerAsn:         peer.ASN,
			AuthPassword:    peer.Password,
			Type:            peerType,
			SendCommunity:   3,
		},
		Timers: &gobgpapi.Timers{Config: timers(peer.TimersProfile)},
		AfiSafis: []*gobgpapi.AfiSafi{
			afiSafi(ipv4Family()),
			afiSafi(ipv6Family()),
		},
		Transport: appliedPeerTransport(peer.PassiveMode),
	}
	if gr := peer.GracefulRestart; gr != nil && gr.Enabled {
		out.GracefulRestart = &gobgpapi.GracefulRestart{Enabled: true, RestartTime: gr.RestartTime, StaleRoutesTime: gr.StaleRoutesTime}
	}
	if peer.EbgpMultihop > 1 {
		out.EbgpMultihop = &gobgpapi.EbgpMultihop{Enabled: true, MultihopTtl: uint32(peer.EbgpMultihop)}
	}
	if peer.RouteReflectorClient {
		out.RouteReflector = &gobgpapi.RouteReflector{
			RouteReflectorClient:    true,
			RouteReflectorClusterId: strings.TrimSpace(peer.RouteReflectorClusterID),
		}
	}
	applyPolicy := &gobgpapi.ApplyPolicy{}
	if len(appliedExportPolicyPrefixes(peer.ExportPolicy)) > 0 && strings.TrimSpace(peer.ExportPolicyName) != "" {
		applyPolicy.ExportPolicy = &gobgpapi.PolicyAssignment{
			Name:          strings.TrimSpace(peer.Address),
			Direction:     gobgpapi.PolicyDirection_POLICY_DIRECTION_EXPORT,
			DefaultAction: gobgpapi.RouteAction_ROUTE_ACTION_REJECT,
			Policies: []*gobgpapi.Policy{{
				Name: strings.TrimSpace(peer.ExportPolicyName),
			}},
		}
	}
	if applyPolicy.ExportPolicy != nil {
		out.ApplyPolicy = applyPolicy
	}
	return out
}

func appliedPeerTransport(passiveMode bool) *gobgpapi.Transport {
	if !passiveMode {
		return nil
	}
	return &gobgpapi.Transport{PassiveMode: true}
}

func timers(profile string) *gobgpapi.TimersConfig {
	switch profile {
	case "fast":
		return &gobgpapi.TimersConfig{ConnectRetry: 1, HoldTime: 9, KeepaliveInterval: 3, IdleHoldTimeAfterReset: 1}
	case "slow":
		return &gobgpapi.TimersConfig{ConnectRetry: 30, HoldTime: 180, KeepaliveInterval: 60, IdleHoldTimeAfterReset: 5}
	default:
		return &gobgpapi.TimersConfig{ConnectRetry: 10, HoldTime: 90, KeepaliveInterval: 30, IdleHoldTimeAfterReset: 1}
	}
}

func afiSafi(family *gobgpapi.Family) *gobgpapi.AfiSafi {
	return &gobgpapi.AfiSafi{
		Config: &gobgpapi.AfiSafiConfig{Family: family, Enabled: true},
		UseMultiplePaths: &gobgpapi.UseMultiplePaths{
			Config: &gobgpapi.UseMultiplePathsConfig{Enabled: true},
			Ebgp:   &gobgpapi.Ebgp{Config: &gobgpapi.EbgpConfig{MaximumPaths: 16}},
		},
	}
}

func appliedPolicies(config bgpdaemon.AppliedConfig) (*gobgpapi.SetPoliciesRequest, *gobgpapi.PolicyAssignment) {
	config = bgpdaemon.Normalize(config)
	req := &gobgpapi.SetPoliciesRequest{}
	assignment := &gobgpapi.PolicyAssignment{
		Name:          "global",
		Direction:     gobgpapi.PolicyDirection_POLICY_DIRECTION_IMPORT,
		DefaultAction: gobgpapi.RouteAction_ROUTE_ACTION_ACCEPT,
	}
	if scopes := appliedEffectiveImportPolicyScopes(config); len(scopes) > 0 {
		name := "routerd-restore-import-effective"
		appendAppliedEffectiveImportPolicy(req, name, scopes)
		assignment.Policies = []*gobgpapi.Policy{{Name: name}}
	}
	for _, policy := range appliedExportPolicies(config) {
		prefixes := appliedExportPolicyPrefixes(policy.Spec)
		if len(prefixes) == 0 {
			continue
		}
		sets := appendPrefixSets(req, policy.Name+"-prefixes", prefixes)
		out := &gobgpapi.Policy{Name: policy.Name}
		for i, prefixSetName := range sets {
			suffix := "allow-export"
			if len(sets) > 1 {
				suffix += strconv.Itoa(i)
			}
			out.Statements = append(out.Statements, &gobgpapi.Statement{
				Name: appliedPolicyStatementName(policy.Name, suffix),
				Conditions: &gobgpapi.Conditions{PrefixSet: &gobgpapi.MatchSet{
					Type: gobgpapi.MatchSet_TYPE_ANY,
					Name: prefixSetName,
				}},
				Actions: &gobgpapi.Actions{RouteAction: gobgpapi.RouteAction_ROUTE_ACTION_ACCEPT},
			})
		}
		req.Policies = append(req.Policies, out)
	}
	return req, assignment
}

func cleanCommunityPolicyValues(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type appliedImportPolicyScope struct {
	Name            string
	Neighbors       []string
	Spec            bgpdaemon.AppliedImportPolicy
	RejectImportAll bool
}

func appliedEffectiveImportPolicyScopes(config bgpdaemon.AppliedConfig) []appliedImportPolicyScope {
	dynamicPrefixes := appliedDynamicPathPrefixes(config.Paths)
	addresses := make([]string, 0, len(config.Peers))
	for address := range config.Peers {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	var scopes []appliedImportPolicyScope
	for _, address := range addresses {
		peer := config.Peers[address]
		address = strings.TrimSpace(peer.Address)
		neighbor := appliedNeighborSetAddress(address)
		if neighbor == "" {
			continue
		}
		spec := peer.ImportPolicy
		// Keep a prefixless per-peer rule such as the direct-mesh
		// local-preference/next-hop policy.  The controller chooses the global
		// default only when the peer has no import policy at all.
		if !appliedImportPolicyConfigured(spec) && !peer.PreserveImportPrefixes && !peer.RejectImportAll {
			spec = config.Global.ImportPolicy
		}
		if !peer.PreserveImportPrefixes {
			spec.AllowedPrefixes = mergeStringSets(spec.AllowedPrefixes, dynamicPrefixes)
		}
		if !appliedImportPolicyConfigured(spec) && !peer.RejectImportAll {
			continue
		}
		scopes = append(scopes, appliedImportPolicyScope{
			Name:            "routerd-restore-import-effective-peer-" + policyPeerName(address),
			Neighbors:       []string{neighbor},
			Spec:            spec,
			RejectImportAll: peer.RejectImportAll,
		})
	}
	return scopes
}

func appliedNeighborSetAddress(value string) string {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return netip.PrefixFrom(address, address.BitLen()).String()
}

func appendAppliedEffectiveImportPolicy(req *gobgpapi.SetPoliciesRequest, policyName string, scopes []appliedImportPolicyScope) {
	statements := make([]*gobgpapi.Statement, 0, len(scopes)*3)
	for _, scope := range scopes {
		neighborSetName := scope.Name + "-neighbors"
		req.DefinedSets = append(req.DefinedSets, &gobgpapi.DefinedSet{
			DefinedType: gobgpapi.DefinedType_DEFINED_TYPE_NEIGHBOR,
			Name:        neighborSetName,
			List:        append([]string(nil), scope.Neighbors...),
		})
		statements = append(statements, appliedEffectiveImportPolicyStatements(req, scope, neighborSetName)...)
	}
	req.Policies = append(req.Policies, &gobgpapi.Policy{Name: policyName, Statements: statements})
}

func appliedEffectiveImportPolicyStatements(req *gobgpapi.SetPoliciesRequest, scope appliedImportPolicyScope, neighborSetName string) []*gobgpapi.Statement {
	prefixes := appliedPolicyPrefixes(scope.Spec)
	sets := appendPrefixSets(req, scope.Name+"-prefixes", prefixes)
	requiredSetName := scope.Name + "-required-communities"
	requiredCommunities := cleanCommunityPolicyValues(scope.Spec.RequiredCommunities)
	if len(requiredCommunities) > 0 {
		req.DefinedSets = append(req.DefinedSets, &gobgpapi.DefinedSet{
			DefinedType: gobgpapi.DefinedType_DEFINED_TYPE_COMMUNITY,
			Name:        requiredSetName,
			List:        requiredCommunities,
		})
	}
	forbiddenSetName := scope.Name + "-forbidden-communities"
	forbiddenCommunities := cleanCommunityPolicyValues(scope.Spec.ForbiddenCommunities)
	if len(forbiddenCommunities) > 0 {
		req.DefinedSets = append(req.DefinedSets, &gobgpapi.DefinedSet{
			DefinedType: gobgpapi.DefinedType_DEFINED_TYPE_COMMUNITY,
			Name:        forbiddenSetName,
			List:        forbiddenCommunities,
		})
	}
	neighborSet := func() *gobgpapi.MatchSet {
		return &gobgpapi.MatchSet{Type: gobgpapi.MatchSet_TYPE_ANY, Name: neighborSetName}
	}
	if scope.RejectImportAll {
		return []*gobgpapi.Statement{{
			Name:       appliedPolicyStatementName(scope.Name, "reject-all-import"),
			Conditions: &gobgpapi.Conditions{NeighborSet: neighborSet()},
			Actions:    &gobgpapi.Actions{RouteAction: gobgpapi.RouteAction_ROUTE_ACTION_REJECT},
		}}
	}
	var statements []*gobgpapi.Statement
	if len(forbiddenCommunities) > 0 {
		statements = append(statements, &gobgpapi.Statement{
			Name: appliedPolicyStatementName(scope.Name, "reject-forbidden-community"),
			Conditions: &gobgpapi.Conditions{
				NeighborSet:  neighborSet(),
				CommunitySet: &gobgpapi.MatchSet{Type: gobgpapi.MatchSet_TYPE_ANY, Name: forbiddenSetName},
			},
			Actions: &gobgpapi.Actions{RouteAction: gobgpapi.RouteAction_ROUTE_ACTION_REJECT},
		})
	}
	if len(sets) == 0 {
		sets = []string{""}
	}
	for i, prefixSetName := range sets {
		acceptConditions := &gobgpapi.Conditions{NeighborSet: neighborSet()}
		if prefixSetName != "" {
			acceptConditions.PrefixSet = &gobgpapi.MatchSet{Type: gobgpapi.MatchSet_TYPE_ANY, Name: prefixSetName}
		}
		if len(requiredCommunities) > 0 {
			acceptConditions.CommunitySet = &gobgpapi.MatchSet{Type: gobgpapi.MatchSet_TYPE_ALL, Name: requiredSetName}
		}
		suffix := "allow-import"
		if len(sets) > 1 {
			suffix += strconv.Itoa(i)
		}
		statements = append(statements, &gobgpapi.Statement{
			Name: appliedPolicyStatementName(scope.Name, suffix), Conditions: acceptConditions,
			Actions: &gobgpapi.Actions{RouteAction: gobgpapi.RouteAction_ROUTE_ACTION_ACCEPT, Nexthop: appliedNextHopAction(scope.Spec), LocalPref: appliedLocalPreferenceAction(scope.Spec)},
		})
	}
	statements = append(statements, &gobgpapi.Statement{
		Name:       appliedPolicyStatementName(scope.Name, "reject-unmatched-import"),
		Conditions: &gobgpapi.Conditions{NeighborSet: neighborSet()},
		Actions:    &gobgpapi.Actions{RouteAction: gobgpapi.RouteAction_ROUTE_ACTION_REJECT},
	})
	return statements
}

// GoBGP prefix sets contain one address family. Splitting a mixed allowlist
// into alternative statements retains its OR semantics; rejecting unmatched
// routes still happens only after all family alternatives have been tried.
func appendPrefixSets(req *gobgpapi.SetPoliciesRequest, name string, prefixes []*gobgpapi.Prefix) []string {
	var v4, v6 []*gobgpapi.Prefix
	for _, p := range prefixes {
		prefix, err := netip.ParsePrefix(p.IpPrefix)
		if err != nil {
			continue
		}
		if prefix.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	var names []string
	for i, group := range [][]*gobgpapi.Prefix{v4, v6} {
		if len(group) == 0 {
			continue
		}
		n := name
		if len(v4) > 0 && len(v6) > 0 {
			if i == 0 {
				n += "-ipv4"
			} else {
				n += "-ipv6"
			}
		}
		req.DefinedSets = append(req.DefinedSets, &gobgpapi.DefinedSet{DefinedType: gobgpapi.DefinedType_DEFINED_TYPE_PREFIX, Name: n, Prefixes: group})
		names = append(names, n)
	}
	return names
}

func appliedDynamicPathPrefixes(paths []bgpdaemon.AppliedPath) []string {
	var out []string
	for _, path := range bgpdaemon.Normalize(bgpdaemon.AppliedConfig{Paths: paths}).Paths {
		if path.Source == bgpdaemon.AppliedPathSourceStatic {
			continue
		}
		if strings.TrimSpace(path.Prefix) == "" {
			continue
		}
		out = append(out, path.Prefix)
	}
	return mergeStringSets(out)
}

func mergeStringSets(groups ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

type appliedExportPolicy struct {
	Name string
	Spec bgpdaemon.AppliedExportPolicy
}

func appliedExportPolicies(config bgpdaemon.AppliedConfig) []appliedExportPolicy {
	byName := map[string]bgpdaemon.AppliedExportPolicy{}
	dynamicPrefixes := appliedDynamicPathPrefixes(config.Paths)
	for _, peer := range config.Peers {
		name := strings.TrimSpace(peer.ExportPolicyName)
		if name == "" {
			continue
		}
		spec := peer.ExportPolicy
		spec.AllowedPrefixes = mergeStringSets(spec.AllowedPrefixes, dynamicPrefixes)
		if len(appliedExportPolicyPrefixes(spec)) == 0 {
			continue
		}
		byName[name] = spec
	}
	var names []string
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	policies := make([]appliedExportPolicy, 0, len(names))
	for _, name := range names {
		policies = append(policies, appliedExportPolicy{Name: name, Spec: byName[name]})
	}
	return policies
}

func appliedPolicyStatementName(policyName, suffix string) string {
	return strings.TrimSpace(policyName) + "-" + suffix
}

func appliedPolicyPrefixes(spec bgpdaemon.AppliedImportPolicy) []*gobgpapi.Prefix {
	var out []*gobgpapi.Prefix
	for _, value := range spec.AllowedPrefixes {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		prefix = prefix.Masked()
		minLen, maxLen := appliedImportPolicyLengthBounds(spec, prefix)
		out = append(out, &gobgpapi.Prefix{IpPrefix: prefix.String(), MaskLengthMin: uint32(minLen), MaskLengthMax: uint32(maxLen)})
	}
	return out
}

func appliedImportPolicyConfigured(spec bgpdaemon.AppliedImportPolicy) bool {
	return len(appliedPolicyPrefixes(spec)) > 0 ||
		len(cleanCommunityPolicyValues(spec.RequiredCommunities)) > 0 ||
		len(cleanCommunityPolicyValues(spec.ForbiddenCommunities)) > 0 ||
		strings.TrimSpace(spec.NextHopRewrite) != "" ||
		spec.LocalPreference != 0
}

func appliedImportPolicyLengthBounds(spec bgpdaemon.AppliedImportPolicy, prefix netip.Prefix) (int, int) {
	minLen := prefix.Bits()
	maxLen := int(appliedPrefixMaxLength(prefix))
	if spec.AllowedPrefixLengthMin > 0 {
		minLen = spec.AllowedPrefixLengthMin
	}
	if spec.AllowedPrefixLengthMax > 0 {
		maxLen = spec.AllowedPrefixLengthMax
	}
	return minLen, maxLen
}

func appliedExportPolicyPrefixes(spec bgpdaemon.AppliedExportPolicy) []*gobgpapi.Prefix {
	return appliedPolicyPrefixes(bgpdaemon.AppliedImportPolicy{AllowedPrefixes: spec.AllowedPrefixes})
}

func appliedPrefixMaxLength(prefix netip.Prefix) uint32 {
	if prefix.Addr().Is6() {
		return 128
	}
	return 32
}

func appliedNextHopAction(spec bgpdaemon.AppliedImportPolicy) *gobgpapi.NexthopAction {
	if strings.TrimSpace(spec.NextHopRewrite) == "unchanged" {
		return &gobgpapi.NexthopAction{Unchanged: true}
	}
	return &gobgpapi.NexthopAction{PeerAddress: true}
}

func appliedLocalPreferenceAction(spec bgpdaemon.AppliedImportPolicy) *gobgpapi.LocalPrefAction {
	if spec.LocalPreference == 0 {
		return nil
	}
	return &gobgpapi.LocalPrefAction{Value: spec.LocalPreference}
}

func validateDynamicMobilityPath(path bgpdaemon.AppliedPath) (bgpdaemon.AppliedPath, error) {
	path = bgpdaemon.NormalizeAppliedPath(path)
	if !bgpdaemon.IsMobilityPathSource(path.Source) {
		return bgpdaemon.AppliedPath{}, fmt.Errorf("dynamic BGP path source %q is not a MobilityPool source", path.Source)
	}
	prefix, err := netip.ParsePrefix(path.Prefix)
	if err != nil {
		return bgpdaemon.AppliedPath{}, fmt.Errorf("dynamic BGP path prefix: %w", err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() || prefix.Bits() != 32 {
		return bgpdaemon.AppliedPath{}, fmt.Errorf("dynamic mobility BGP paths must be IPv4 /32, got %s", prefix.String())
	}
	path.Prefix = prefix.String()
	path.Family = bgpdaemon.AppliedPathFamilyIPv4Unicast
	if err := bgpdaemon.ValidateAppliedPath(path); err != nil {
		return bgpdaemon.AppliedPath{}, err
	}
	return path, nil
}

func refreshDynamicPathPolicies(ctx context.Context, server pathServer, applied bgpdaemon.AppliedConfig) error {
	policyServer, ok := server.(policyPathServer)
	if !ok {
		return nil
	}
	if err := applyAppliedPolicies(ctx, policyServer, applied); err != nil {
		return err
	}
	withheld := pendingDirectPeerTransitions(applied)
	for _, peer := range sortedPeers(applied.Peers) {
		if peer.PreserveImportPrefixes || withheld[peer.Address] {
			continue
		}
		// Replacing a policy does not by itself evict previously accepted RIB
		// entries. Re-evaluate regular peers after a dynamic prefix is removed;
		// direct peers retain their separately fenced, narrow ownership policy.
		if err := policyServer.ResetPeer(ctx, &gobgpapi.ResetPeerRequest{Address: peer.Address, Soft: true, Direction: gobgpapi.ResetPeerRequest_DIRECTION_IN}); err != nil {
			return fmt.Errorf("soft reset import policy for peer %s: %w", peer.Address, err)
		}
	}
	for _, address := range dynamicExportPolicyPeerAddresses(applied) {
		if err := policyServer.ResetPeer(ctx, &gobgpapi.ResetPeerRequest{
			Address:   address,
			Soft:      true,
			Direction: gobgpapi.ResetPeerRequest_DIRECTION_OUT,
		}); err != nil {
			return fmt.Errorf("soft reset export policy for peer %s: %w", address, err)
		}
	}
	return nil
}

func dynamicExportPolicyPeerAddresses(applied bgpdaemon.AppliedConfig) []string {
	applied = bgpdaemon.Normalize(applied)
	withheld := pendingDirectPeerTransitions(applied)
	var addresses []string
	for address, peer := range applied.Peers {
		if withheld[address] {
			continue
		}
		if strings.TrimSpace(peer.ExportPolicyName) == "" {
			continue
		}
		peerAddress := strings.TrimSpace(peer.Address)
		if peerAddress == "" {
			peerAddress = strings.TrimSpace(address)
		}
		if peerAddress == "" {
			continue
		}
		addresses = append(addresses, peerAddress)
	}
	sort.Strings(addresses)
	return addresses
}

func isMissingGoBGPPath(err error) bool {
	return err != nil && strings.Contains(err.Error(), "can't find a specified path")
}

func httpStatusForPathError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	msg := err.Error()
	if strings.Contains(msg, "not initialized") {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func localPath(prefix string) (*gobgpapi.Path, error) {
	return pathFromAppliedPath(bgpdaemon.StaticAppliedPath(prefix, nil))
}

func pathFromAppliedPath(appliedPath bgpdaemon.AppliedPath) (*gobgpapi.Path, error) {
	appliedPath = bgpdaemon.NormalizeAppliedPath(appliedPath)
	parsed, err := netip.ParsePrefix(appliedPath.Prefix)
	if err != nil {
		return nil, err
	}
	parsed = parsed.Masked()
	nlri := ipAddressNLRI(parsed)
	attrs := []*gobgpapi.Attribute{originAttribute()}
	nextHop := "0.0.0.0"
	if parsed.Addr().Is6() {
		nextHop = "::"
	}
	if appliedPath.Attrs.NextHop != "" {
		nextHop = appliedPath.Attrs.NextHop
	}
	if parsed.Addr().Is6() {
		attrs = append(attrs, &gobgpapi.Attribute{Attr: &gobgpapi.Attribute_MpReach{MpReach: &gobgpapi.MpReachNLRIAttribute{Family: ipv6Family(), NextHops: []string{nextHop}, Nlris: []*gobgpapi.NLRI{nlri}}}})
	} else {
		attrs = append(attrs, nextHopAttribute(nextHop))
	}
	if appliedPath.Attrs.LocalPref > 0 {
		attrs = append(attrs, localPrefAttribute(appliedPath.Attrs.LocalPref))
	}
	if appliedPath.Attrs.MED > 0 {
		attrs = append(attrs, medAttribute(appliedPath.Attrs.MED))
	}
	communities, err := standardCommunities(appliedPath.Attrs.Communities)
	if err != nil {
		return nil, err
	}
	if len(communities) > 0 {
		attrs = append(attrs, communitiesAttribute(communities))
	}
	return &gobgpapi.Path{Family: familyForPrefix(parsed), Nlri: nlri, Pattrs: attrs}, nil
}

func ipAddressNLRI(prefix netip.Prefix) *gobgpapi.NLRI {
	return &gobgpapi.NLRI{Nlri: &gobgpapi.NLRI_Prefix{Prefix: &gobgpapi.IPAddressPrefix{
		Prefix: prefix.Addr().String(), PrefixLen: uint32(prefix.Bits()),
	}}}
}

func originAttribute() *gobgpapi.Attribute {
	return &gobgpapi.Attribute{Attr: &gobgpapi.Attribute_Origin{Origin: &gobgpapi.OriginAttribute{Origin: 0}}}
}

func nextHopAttribute(nextHop string) *gobgpapi.Attribute {
	return &gobgpapi.Attribute{Attr: &gobgpapi.Attribute_NextHop{NextHop: &gobgpapi.NextHopAttribute{NextHop: nextHop}}}
}

func localPrefAttribute(localPref uint32) *gobgpapi.Attribute {
	return &gobgpapi.Attribute{Attr: &gobgpapi.Attribute_LocalPref{LocalPref: &gobgpapi.LocalPrefAttribute{LocalPref: localPref}}}
}

func medAttribute(med uint32) *gobgpapi.Attribute {
	return &gobgpapi.Attribute{Attr: &gobgpapi.Attribute_MultiExitDisc{MultiExitDisc: &gobgpapi.MultiExitDiscAttribute{Med: med}}}
}

func communitiesAttribute(communities []uint32) *gobgpapi.Attribute {
	return &gobgpapi.Attribute{Attr: &gobgpapi.Attribute_Communities{Communities: &gobgpapi.CommunitiesAttribute{Communities: communities}}}
}

func standardCommunities(values []string) ([]uint32, error) {
	var out []uint32
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.Contains(value, ":") {
			left, right, ok := strings.Cut(value, ":")
			if !ok {
				return nil, fmt.Errorf("invalid standard community %q", value)
			}
			hi, err := strconv.ParseUint(strings.TrimSpace(left), 10, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid standard community %q: %w", value, err)
			}
			lo, err := strconv.ParseUint(strings.TrimSpace(right), 10, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid standard community %q: %w", value, err)
			}
			out = append(out, uint32(hi)<<16|uint32(lo))
			continue
		}
		community, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid standard community %q: %w", value, err)
		}
		out = append(out, uint32(community))
	}
	return out, nil
}

func familyForPrefix(prefix netip.Prefix) *gobgpapi.Family {
	if prefix.Addr().Is6() {
		return ipv6Family()
	}
	return ipv4Family()
}

func ipv4Family() *gobgpapi.Family {
	return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP, Safi: gobgpapi.Family_SAFI_UNICAST}
}

func ipv6Family() *gobgpapi.Family {
	return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP6, Safi: gobgpapi.Family_SAFI_UNICAST}
}

func sortedPeers(peers map[string]bgpdaemon.AppliedPeer) []bgpdaemon.AppliedPeer {
	var keys []string
	for key := range peers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]bgpdaemon.AppliedPeer, 0, len(keys))
	for _, key := range keys {
		out = append(out, peers[key])
	}
	return out
}
