// Package serviceruntime connects protocol engines to the control plane. It
// never owns netlink mutations. The PPP engine owns its negotiated PPP unit;
// routes and forwarding always belong to the network writer.
package serviceruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelay"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelayconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserver"
	"github.com/imksoo/routerd2/internal/protocol/dns"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/pppoe"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/worker"
)

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}

func Run(ctx context.Context, runtimeDir, name, role, path, digest string, generation uint64, ready func()) error {
	if generation == 0 {
		return errors.New("service generation is required")
	}
	instanceID := os.Getenv(worker.SupervisorInstanceEnv)
	if !serviceconfig.ValidSupervisorInstance(instanceID) {
		return errors.New("supervisor instance is required")
	}
	if path != filepath.Join(runtimeDir, "instances", name, digest+".json") {
		return errors.New("unexpected instance configuration path")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
	_ = f.Close()
	if err != nil {
		return err
	}
	if len(b) > 2<<20 {
		return errors.New("instance config exceeds 2 MiB")
	}
	hash := sha256.Sum256(b)
	if hex.EncodeToString(hash[:]) != digest {
		return errors.New("instance configuration digest mismatch")
	}
	var instance serviceconfig.Instance
	if err := decode(b, &instance); err != nil {
		return err
	}
	if instance.Name != name || instance.Role != role {
		return errors.New("instance identity mismatch")
	}
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		return err
	}
	c := &effectsClient{client: localapi.NewClient(filepath.Join(runtimeDir, "control.sock")), identity: serviceconfig.Update{SupervisorInstance: instanceID, Name: name, Role: role, ConfigDigest: digest, Generation: generation, ProducerEpoch: hex.EncodeToString(epoch[:])}}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Lease-owning HA engines refresh their own acknowledged deadline. A
	// generic replay must never keep a dead election alive.
	if role == "dhcp4" || role == "dhcp6" || role == "ra" || role == "ra_client" {
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		go func() { defer close(done); c.replay(runCtx) }()
	}
	switch role {
	case "pppoe":
		var cfg pppoeconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		engine := os.NewFile(5, "pppoe-engine")
		defer engine.Close()
		return pppoe.Run(runCtx, cfg, engine, func(ctx context.Context, s pppoeconfig.Snapshot) error { return c.update(ctx, s) }, ready)
	case "health":
		var cfg health.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return c.runHealth(runCtx, runtimeDir, instance.EventDirectory, cfg, ready)
	case "bfd":
		var cfg bfd.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return bfd.Run(runCtx, cfg, bfd.EffectsFunc(func(ctx context.Context, snapshot bfd.Snapshot) error { return c.update(ctx, snapshot) }), ready)
	case "aftr":
		return c.runAFTR(runCtx, instance, digest, ready)
	case "ra_client":
		var cfg raclient.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return raclient.Run(runCtx, cfg, raclient.EffectsFunc(func(ctx context.Context, snapshot raclient.Snapshot) error { return c.update(ctx, snapshot) }), ready)
	case "dhcp_relay":
		var cfg dhcprelayconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return dhcprelay.Run(runCtx, cfg, ready)
	case "dhcp_server":
		var cfg dhcpserver.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return dhcpserver.Run(runCtx, cfg, dhcpserver.EffectsFunc(func(ctx context.Context, s dhcpserver.Snapshot) error { return c.update(ctx, s) }), ready)
	case "bgp":
		return c.runBGP(runCtx, instance, digest, ready)
	case "dhcp4":
		var cfg dhcp4.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return dhcp4.Run(runCtx, cfg, dhcp4.EffectsFunc(func(ctx context.Context, s dhcp4.Snapshot) error { return c.update(ctx, s) }), ready)
	case "dhcp6":
		var cfg dhcp6config.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return dhcp6.Run(runCtx, cfg, dhcp6.EffectsFunc(func(ctx context.Context, s dhcp6config.Snapshot) error { return c.update(ctx, s) }), ready)
	case "ra":
		var cfg ra.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		effects := ra.EffectsFunc(func(ctx context.Context, s ra.Snapshot) error { return c.update(ctx, s) })
		if !instance.DynamicRA {
			return ra.Run(runCtx, cfg, effects, ready)
		}
		// Fetch before opening the protocol socket: a generated RA worker may
		// only advertise the controller's acknowledged, finite PD projection.
		if err := c.client.Do(runCtx, "GET", "/v1/services/"+name+"/ra", nil, &cfg); err != nil {
			return err
		}
		updates := make(chan ra.Config, 1)
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		go func() { defer close(done); defer close(updates); c.watchRA(runCtx, name, updates) }()
		return ra.RunWithUpdates(runCtx, cfg, updates, effects, ready)
	case "dns":
		var cfg dns.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		if !instance.DynamicDNS {
			return dns.Run(runCtx, cfg, ready)
		}
		var initial dnsconfig.LeaseProjection
		if err := c.client.Do(runCtx, "GET", "/v1/services/"+name+"/dns-leases", nil, &initial); err != nil {
			return err
		}
		if initial.ConfigDigest != digest {
			return errors.New("DNS lease projection belongs to a different configuration")
		}
		updates := make(chan dnsconfig.LeaseProjection, 1)
		updates <- initial
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		go func() { defer close(done); defer close(updates); c.watchDNSLeases(runCtx, name, digest, updates) }()
		return dns.RunWithLeaseUpdates(runCtx, cfg, updates, ready)
	case "vrrp":
		var cfg vrrp.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return vrrp.Run(runCtx, cfg, vrrpEffects{client: c}, ready)
	default:
		return fmt.Errorf("unavailable bundled service role %q", role)
	}
}

func (c *effectsClient) watchRA(ctx context.Context, name string, updates chan ra.Config) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var cfg ra.Config
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.client.Do(call, "GET", "/v1/services/"+name+"/ra", nil, &cfg)
		cancel()
		if err != nil {
			continue
		} // The previous absolute lifetimes keep decaying.
		select {
		case updates <- cfg:
		case <-ctx.Done():
			return
		default:
			// A slow advertisement loop needs the newest complete projection.
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- cfg:
			case <-ctx.Done():
				return
			}
		}
	}
}

type effectsClient struct {
	mu       sync.Mutex
	client   *localapi.Client
	identity serviceconfig.Update
	latest   json.RawMessage
}

func (c *effectsClient) update(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.send(ctx, b); err != nil {
		return err
	}
	c.latest = append(c.latest[:0], b...)
	return nil
}
func (c *effectsClient) send(ctx context.Context, b json.RawMessage) (serviceconfig.ApplicationReceipt, error) {
	c.identity.Sequence++
	r := c.identity
	r.Snapshot = b
	callCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	if c.identity.Role == "vrrp" || c.identity.Role == "bgp" {
		var ack serviceconfig.ApplicationReceipt
		if err := c.client.Do(callCtx, "POST", "/v1/services/effects", r, &ack); err != nil {
			return ack, err
		}
		if !ack.Applied {
			return ack, errors.New("protocol reply does not acknowledge kernel application")
		}
		return ack, nil
	}
	return serviceconfig.ApplicationReceipt{}, c.client.Do(callCtx, "POST", "/v1/services/state", r, nil)
}
func (c *effectsClient) replay(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			if len(c.latest) > 0 {
				_, _ = c.send(ctx, c.latest)
			}
			c.mu.Unlock()
		}
	}
}

func (c *effectsClient) watchDNSLeases(ctx context.Context, name, digest string, updates chan dnsconfig.LeaseProjection) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var projection dnsconfig.LeaseProjection
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.client.Do(call, "GET", "/v1/services/"+name+"/dns-leases", nil, &projection)
		cancel()
		if err != nil || projection.ConfigDigest != digest {
			continue // Existing ownership and lease deadlines continue to expire.
		}
		select {
		case updates <- projection:
		case <-ctx.Done():
			return
		default:
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- projection:
			case <-ctx.Done():
				return
			}
		}
	}
}
