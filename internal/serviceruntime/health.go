package serviceruntime

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
)

func (c *effectsClient) runHealth(ctx context.Context, dir string, cfg health.Config, ready func()) error {
	networkClient := localapi.NewClient(filepath.Join(dir, "network.sock"))
	probe := func(ctx context.Context, cfg health.Config) old.ProbeResult {
		result := health.Probe(ctx, cfg)
		// Evidence is best effort, and cannot turn a successful scoped probe into
		// failure. The network worker alone performs the Netlink route lookup.
		evidenceCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		var route network.RouteLookupResult
		req := network.RouteLookupRequest{Target: cfg.Target, Interface: cfg.Interface, Source: cfg.SourceAddress, Mark: cfg.Mark}
		if networkClient.Do(evidenceCtx, http.MethodPost, "/v1/routes/lookup", req, &route) == nil {
			result.OutInterface = route.Interface
			result.NextHop = route.Gateway
			result.RouteSource = "netlink"
			if result.SourceAddress == "" {
				result.SourceAddress = route.Source
			}
		}
		return result
	}
	return health.RunWithProbe(ctx, cfg, func(ctx context.Context, s health.Snapshot) error { return c.update(ctx, s) }, ready, probe)
}
