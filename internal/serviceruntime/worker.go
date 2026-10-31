// Package serviceruntime connects protocol engines to the control plane. It
// delegates PPP units and configured conntrack replication to bundled engines.
// Addresses, routes and firewall rules belong to the network writer.
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
	"runtime"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/conntracksync"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelay"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelayconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserver"
	"github.com/imksoo/routerd2/internal/protocol/dns"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/firewalllog"
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/otlplogconfig"
	"github.com/imksoo/routerd2/internal/protocol/otlplogs"
	"github.com/imksoo/routerd2/internal/protocol/pppoe"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/samadmission"
	"github.com/imksoo/routerd2/internal/protocol/samadmissionconfig"
	"github.com/imksoo/routerd2/internal/protocol/samclient"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
	"github.com/imksoo/routerd2/internal/protocol/syslog"
	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
	"github.com/imksoo/routerd2/internal/protocol/tailscale"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
	"github.com/imksoo/routerd2/internal/protocol/trafficflow"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/protocol/webconsole"
	"github.com/imksoo/routerd2/internal/protocol/webconsoleconfig"
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
	switch role {
	case "web_console":
		var cfg webconsoleconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return webconsole.Run(runCtx, cfg, runtimeDir, ready)
	case "traffic_flow":
		var cfg trafficflowconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return trafficflow.Run(runCtx, cfg, ready)
	case "firewall_log":
		var cfg firewalllogconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return firewalllog.Run(runCtx, cfg, ready)
	case "otlp_logs":
		var cfg otlplogconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return otlplogs.Run(runCtx, cfg, runtimeDir, ready)
	case "sam_admission":
		var cfg samadmissionconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return samadmission.Run(runCtx, cfg, ready)
	case "sam_client":
		var cfg samclientconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return samclient.Run(runCtx, cfg, func(ctx context.Context, s samclientconfig.Snapshot) error { return c.update(ctx, s) }, ready)
	case "syslog":
		var cfg syslogconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return syslog.Run(runCtx, cfg, runtimeDir, ready)
	case "ntp":
		return c.runNTP(runCtx, instance, ready)
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
		return raclient.Run(runCtx, cfg, raClientEffects{client: c}, ready)
	case "dhcp_relay":
		var cfg dhcprelayconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		return dhcprelay.Run(runCtx, cfg, ready)
	case "dhcp6_server":
		return c.runDHCP6Server(runCtx, instance, digest, ready)
	case "dhcp_server":
		var cfg dhcpserver.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		engine := os.NewFile(5, "dnsmasq-engine")
		defer engine.Close()
		return dhcpserver.Run(runCtx, cfg, engine, func(ctx context.Context, s dhcpserver.Snapshot) error { return c.update(ctx, s) }, ready)
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
		var updates chan dhcp6config.RouterFlags
		if cfg.UseRA {
			updates = make(chan dhcp6config.RouterFlags, 1)
			done := make(chan struct{})
			defer func() { cancel(); <-done }()
			go func() { defer close(done); c.watchDHCP6RA(runCtx, name, updates) }()
		}
		return dhcp6.Run(runCtx, cfg, dhcp6Effects{client: c}, updates, ready)
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
		if !dnsconfig.NeedsInput(cfg.Runtime) {
			return dns.Run(runCtx, cfg, nil, func(ctx context.Context, s dnsconfig.Snapshot) error { return c.update(ctx, s) }, ready)
		}
		var initial dnsconfig.Input
		if err := c.client.Do(runCtx, "GET", "/v1/services/"+name+"/dns-input", nil, &initial); err != nil {
			return err
		}
		if initial.ConfigDigest != digest {
			return errors.New("DNS input belongs to a different configuration")
		}
		updates := make(chan dnsconfig.Input, 1)
		updates <- initial
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		go func() { defer close(done); defer close(updates); c.watchDNSInput(runCtx, name, initial, updates) }()
		return dns.Run(runCtx, cfg, updates, func(ctx context.Context, s dnsconfig.Snapshot) error { return c.update(ctx, s) }, ready)
	case "tailscale":
		var cfg tailscaleconfig.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		image := os.NewFile(5, "tailscale-engine")
		defer image.Close()
		return tailscale.Run(runCtx, cfg, image, func(ctx context.Context) (tailscaleconfig.Input, error) {
			var input tailscaleconfig.Input
			err := c.client.Do(ctx, "GET", "/v1/services/"+name+"/tailscale-input", nil, &input)
			if err == nil && input.ConfigDigest != digest {
				err = errors.New("Tailscale input belongs to another configuration")
			}
			return input, err
		}, func(ctx context.Context, s tailscaleconfig.Snapshot) error { return c.update(ctx, s) }, ready)
	case "vrrp":
		var cfg vrrp.Config
		if err := decode(instance.Config, &cfg); err != nil {
			return err
		}
		if cfg.ConntrackSync == nil {
			return vrrp.Run(runCtx, cfg, vrrpEffects{client: c}, ready)
		}
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		image := os.NewFile(5, "conntrackd-engine")
		defer image.Close()
		sync, err := conntracksync.Start(runCtx, *cfg.ConntrackSync, image)
		if err != nil {
			return err
		}
		defer sync.Close()
		err = vrrp.Run(sync.Context(), cfg, syncedVRRPEffects{effects: vrrpEffects{client: c}, sync: sync}, ready)
		return errors.Join(err, sync.Err())
	default:
		return fmt.Errorf("unavailable bundled service role %q", role)
	}
}

func (c *effectsClient) watchRA(ctx context.Context, name string, updates chan ra.Config) {
	ticker := time.NewTicker(200 * time.Millisecond)
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
}

type dhcp6Effects struct{ client *effectsClient }

func (e dhcp6Effects) Update(ctx context.Context, s dhcp6config.Snapshot) error {
	return e.client.update(ctx, s)
}

func (e dhcp6Effects) Withdraw(ctx context.Context, s dhcp6config.Snapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return e.client.withdraw(ctx, b)
}

type raClientEffects struct{ client *effectsClient }

func (e raClientEffects) Update(ctx context.Context, s raclient.Snapshot) error {
	return e.client.update(ctx, s)
}
func (e raClientEffects) Withdraw(ctx context.Context, s raclient.Snapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return e.client.withdraw(ctx, b)
}

func (c *effectsClient) withdraw(ctx context.Context, b json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.identity.Sequence++
	u := c.identity
	u.Snapshot = b
	call, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var receipt serviceconfig.ApplicationReceipt
	if err := c.client.Do(call, "POST", "/v1/services/effects", u, &receipt); err != nil {
		return err
	}
	if !receipt.Applied {
		return errors.New("protocol withdrawal was not applied")
	}
	return nil
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

func (c *effectsClient) watchDNSInput(ctx context.Context, name string, current dnsconfig.Input, updates chan dnsconfig.Input) {
	for {
		// Conditional producers can have less than a second of authority.
		// Poll before the original endpoint deadline, without extending it.
		delay := time.Second
		if clock, err := boottime.Read(); err == nil {
			for _, address := range current.Listeners {
				if remaining := address.ValidUntil.Remaining(clock); remaining > 0 {
					delay = min(delay, max(50*time.Millisecond, remaining/2))
				}
			}
			for _, record := range current.Records {
				if remaining := record.ValidUntil.Remaining(clock); remaining > 0 {
					delay = min(delay, max(50*time.Millisecond, remaining/2))
				}
			}
			for _, servers := range [][]dnsconfig.LearnedUpstream{current.Upstreams, current.Bootstraps} {
				for _, server := range servers {
					if remaining := server.ValidUntil.Remaining(clock); remaining > 0 {
						delay = min(delay, max(50*time.Millisecond, remaining/2))
					}
				}
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		var projection dnsconfig.Input
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.client.Do(call, "GET", "/v1/services/"+name+"/dns-input", nil, &projection)
		cancel()
		if err != nil || projection.ConfigDigest != current.ConfigDigest {
			continue // Existing ownership and lease deadlines continue to expire.
		}
		current = projection
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

func (c *effectsClient) watchDHCP6RA(ctx context.Context, name string, updates chan dhcp6config.RouterFlags) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var flags dhcp6config.RouterFlags
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.client.Do(call, "GET", "/v1/services/"+name+"/dhcp6-input", nil, &flags)
		cancel()
		if err == nil {
			select {
			case updates <- flags:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
