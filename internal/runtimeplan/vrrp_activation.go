package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

var ErrVRRPActivationPending = errors.New("VRRP awaits admitted VIP publication")

type vrrpPublication struct {
	generation, transition                             uint64
	producerEpoch, electionEpoch, configDigest, bootID string
}
type vrrpAdmission struct {
	publication           vrrpPublication
	sourceEpoch, sequence uint64
	grant                 vrrp.ActivationGrant
}

func publicationFor(u serviceconfig.Update, s vrrp.Snapshot) vrrpPublication {
	return vrrpPublication{generation: u.Generation, transition: s.Transition, producerEpoch: u.ProducerEpoch, electionEpoch: s.Epoch, configDigest: u.ConfigDigest, bootID: s.BootID}
}
func (r *Runtime) publicationLatchedLocked(u serviceconfig.Update, s vrrp.Snapshot) bool {
	key, ok := r.vrrpPublications[u.Name]
	return ok && key == publicationFor(u, s)
}
func (r *Runtime) gracefulVRRPConfigLocked(name string) (vrrp.Config, bool) {
	binding, ok := r.bindings[name]
	if !ok || binding.instance.Role != "vrrp" {
		return vrrp.Config{}, false
	}
	var c vrrp.Config
	if strict(binding.instance.Config, &c) != nil || c.GracefulActivation == nil {
		return vrrp.Config{}, false
	}
	return c, true
}
func (r *Runtime) validateVRRPAdmissionLocked(u serviceconfig.Update) error {
	if _, gated := r.gracefulVRRPConfigLocked(u.Name); !gated {
		return nil
	}
	var s vrrp.Snapshot
	if err := json.Unmarshal(u.Snapshot, &s); err != nil {
		return err
	}
	if !s.Active {
		return nil
	}
	if r.publicationLatchedLocked(u, s) {
		return nil
	}
	admission, ok := r.vrrpAdmissions[u.Name]
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	if !ok || admission.sourceEpoch != r.sourceEpoch || admission.publication != publicationFor(u, s) || admission.sequence == math.MaxUint64 || u.Sequence != admission.sequence+1 || s.ActivationGrant != admission.grant || s.ActivationGrant.BootID != clock.BootID || uint64(clock.BoottimeNanoseconds) >= s.ActivationGrant.ExpiresNanos || s.ObservedNanos < s.ActivationGrant.ObservedNanos {
		return errors.New("VRRP publication requires a current control-issued admission grant")
	}
	c, _ := r.gracefulVRRPConfigLocked(u.Name)
	facts, factErr := r.conditionFactsLocked(r.source, r.revision, time.Now())
	if factErr != nil {
		return factErr
	}
	ready, until := facts.ConditionAuthority(c.GracefulActivation.ReadyWhen)
	if ready != resourceprogram.True || until < uint64(clock.BoottimeNanoseconds) {
		return errors.New("VRRP activation readiness is no longer current")
	}
	return nil
}

func (r *Runtime) GrantVRRPActivation(ctx context.Context, u serviceconfig.Update) (vrrp.ActivationGrant, error) {
	var denied vrrp.ActivationGrant
	if err := ctx.Err(); err != nil {
		return denied, err
	}
	if u.SupervisorInstance != r.supervisorInstance || u.Role != "vrrp" {
		return denied, ErrStale
	}
	status, err := r.manager.Status(ctx)
	if err != nil {
		return denied, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configured || status.InstanceID != r.supervisorInstance || status.Stopping {
		return denied, ErrStale
	}
	c, gated := r.gracefulVRRPConfigLocked(u.Name)
	current, ok := r.records[u.Name]
	if !gated || !ok || !sameVRRPUpdate(current, u) {
		return denied, ErrStale
	}
	matched := false
	for _, module := range status.Modules {
		if module.Name == u.Name && module.Role == "vrrp" && module.ConfigDigest == u.ConfigDigest && module.Generation == u.Generation {
			matched = true
			break
		}
	}
	if !matched {
		return denied, ErrStale
	}
	var s vrrp.Snapshot
	if err := strict(u.Snapshot, &s); err != nil {
		return denied, err
	}
	clock, err := boottime.Read()
	if err != nil {
		return denied, err
	}
	if s.Active || !vrrp.CurrentElection(s, clock) {
		return denied, ErrStale
	}
	r.installGuardStatusLocked(status, nil)
	facts, err := r.conditionFactsLocked(r.source, r.revision, time.Now())
	if err != nil {
		return denied, err
	}
	ready, until := facts.ConditionAuthority(c.GracefulActivation.ReadyWhen)
	now := uint64(clock.BoottimeNanoseconds)
	until = min(until, s.ExpiresNanos, r.guardUntilNanos)
	if ready != resourceprogram.True || until <= now {
		return denied, nil
	}
	if err := ctx.Err(); err != nil {
		return denied, err
	}
	if r.vrrpAdmissionID == math.MaxUint64 {
		return denied, errors.New("VRRP activation grant sequence exhausted")
	}
	r.vrrpAdmissionID++
	grant := vrrp.ActivationGrant{ID: r.vrrpAdmissionID, BootID: s.BootID, ElectionEpoch: s.Epoch, Transition: s.Transition, ObservedNanos: s.ObservedNanos, ExpiresNanos: until}
	if r.vrrpAdmissions == nil {
		r.vrrpAdmissions = make(map[string]vrrpAdmission)
	}
	r.vrrpAdmissions[u.Name] = vrrpAdmission{publication: publicationFor(u, s), sourceEpoch: r.sourceEpoch, sequence: u.Sequence, grant: grant}
	return grant, nil
}

func (r *Runtime) vrrpActivationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/services/vrrp-activation", func(w http.ResponseWriter, req *http.Request) {
		var u serviceconfig.Update
		if err := localapi.Decode(w, req, &u); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		grant, err := r.GrantVRRPActivation(req.Context(), u)
		if err != nil {
			code := 422
			if errors.Is(err, ErrStale) {
				code = 409
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, grant)
	})
}

// Before first admission a VIP receives only the short grant lifetime. Once
// that kernel transaction ACKs while current, ownership may be promoted to the
// original election deadline. Expired permission can never create a long VIP.
func (r *Runtime) restrictVRRPPublicationLocked(name string, u serviceconfig.Update, s vrrp.Snapshot) uint64 {
	if _, gated := r.gracefulVRRPConfigLocked(name); !gated || r.publicationLatchedLocked(u, s) {
		return s.ExpiresNanos
	}
	return min(s.ExpiresNanos, s.ActivationGrant.ExpiresNanos)
}

func (r *Runtime) applyVRRPProjectionLocked(ctx context.Context, p projection) (result projection, resultErr error) {
	newlyAdmitted := make([]string, 0)
	defer func() {
		if resultErr == nil {
			return
		}
		for _, name := range newlyAdmitted {
			delete(r.vrrpPublications, name)
		}
		for name, u := range r.records {
			if _, gated := r.gracefulVRRPConfigLocked(name); !gated {
				continue
			}
			var s vrrp.Snapshot
			if json.Unmarshal(u.Snapshot, &s) == nil && s.Active && !r.publicationLatchedLocked(u, s) {
				delete(r.vrrpAdmissions, name)
			}
		}
	}()
	if err := r.applyNetworkLocked(ctx, p.network); err != nil {
		return p, err
	}
	for name, u := range r.records {
		if _, gated := r.gracefulVRRPConfigLocked(name); !gated {
			continue
		}
		var s vrrp.Snapshot
		if json.Unmarshal(u.Snapshot, &s) != nil || !s.Active || r.publicationLatchedLocked(u, s) {
			continue
		}
		if err := r.validateVRRPAdmissionLocked(u); err != nil {
			return p, err
		}
		clock, err := boottime.Read()
		if err != nil {
			return p, err
		}
		provisional := s
		provisional.ExpiresNanos = min(s.ExpiresNanos, s.ActivationGrant.ExpiresNanos)
		if !vrrp.CurrentOwnership(provisional, clock) || vrrpEffectApplied(uWithSnapshot(u, provisional), p.network) != nil {
			continue
		}
		if r.vrrpPublications == nil {
			r.vrrpPublications = make(map[string]vrrpPublication)
		}
		r.vrrpPublications[name] = publicationFor(u, s)
		newlyAdmitted = append(newlyAdmitted, name)
		for i := range p.network.Interfaces {
			iface := &p.network.Interfaces[i]
			if iface.Name != s.VIPInterface {
				continue
			}
			for _, address := range s.Addresses {
				opts := iface.AddressOptions[address]
				opts.BootLifetime = network.BootLifetime{BootID: s.BootID, PreferredNanos: s.ExpiresNanos, ValidNanos: s.ExpiresNanos}
				iface.AddressOptions[address] = opts
			}
		}
	}
	if len(newlyAdmitted) > 0 {
		if err := r.applyNetworkLocked(ctx, p.network); err != nil {
			for _, name := range newlyAdmitted {
				delete(r.vrrpPublications, name)
			}
			return p, err
		}
	}
	return p, nil
}
func uWithSnapshot(u serviceconfig.Update, s vrrp.Snapshot) serviceconfig.Update {
	u.Snapshot, _ = json.Marshal(s)
	return u
}

func (r *Runtime) revokeVRRPPublicationLocked(u serviceconfig.Update) {
	if u.Role != "vrrp" {
		return
	}
	var s vrrp.Snapshot
	if json.Unmarshal(u.Snapshot, &s) != nil {
		return
	}
	if !s.Active {
		delete(r.vrrpAdmissions, u.Name)
	}
	if !s.Active || !r.publicationLatchedLocked(u, s) {
		delete(r.vrrpPublications, u.Name)
	}
}

func (r *Runtime) pruneVRRPAuthoritiesLocked() {
	for name, publication := range r.vrrpPublications {
		u, ok := r.records[name]
		var s vrrp.Snapshot
		binding, bound := r.bindings[name]
		if !ok || !bound || binding.digest != publication.configDigest || json.Unmarshal(u.Snapshot, &s) != nil || !s.Active || publicationFor(u, s) != publication {
			delete(r.vrrpPublications, name)
		}
	}
	for name, admission := range r.vrrpAdmissions {
		binding, ok := r.bindings[name]
		if !ok || binding.digest != admission.publication.configDigest || admission.sourceEpoch != r.sourceEpoch {
			delete(r.vrrpAdmissions, name)
		}
	}
}
