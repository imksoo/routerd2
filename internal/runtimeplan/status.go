package runtimeplan

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

const statusSnapshotLimit = 4096
const statusRouteLimit = 128

// List responses must remain usable with every configured instance at its
// individual snapshot limit. Larger snapshots are available through the
// instance endpoint; the list explicitly reports their presence and size.
func (r *Runtime) State() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Status{RouteIssueCount: len(r.routeIssues), RouteIssues: boundedIssues(r.routeIssues, 0, statusRouteLimit), Revision: r.revision, Phase: r.phase, LastError: boundedText(r.lastError, 4096), Instances: []InstanceStatus{}}
	for name, b := range r.bindings {
		s.Instances = append(s.Instances, r.instanceStatusLocked(name, b, r.records[name], false))
	}
	sort.Slice(s.Instances, func(i, j int) bool { return s.Instances[i].Name < s.Instances[j].Name })
	return s
}

func (r *Runtime) instanceStatusLocked(name string, b binding, v serviceconfig.Update, detail bool) InstanceStatus {
	out := InstanceStatus{Name: name, Role: b.instance.Role, ConfigDigest: b.digest, Generation: v.Generation, ProducerEpoch: v.ProducerEpoch, Sequence: v.Sequence, SnapshotBytes: len(v.Snapshot)}
	encoded, err := json.Marshal(v.Snapshot) // accounts for JSON/HTML escaping
	if len(v.Snapshot) > 0 && err == nil {
		if detail || len(encoded) <= statusSnapshotLimit {
			out.Snapshot = encoded
		} else {
			out.SnapshotTruncated = true
		}
	}
	if v.Role == "bgp" || v.Role == "vrrp" || v.Role == "dhcp_server" {
		var grant struct {
			Active bool `json:"active"`
		}
		if json.Unmarshal(v.Snapshot, &grant) == nil && grant.Active {
			out.OwnershipExpired = !time.Now().Before(r.leases[name])
		}
	}
	return out
}

func boundedText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func boundedIssues(all []RouteIssue, offset, limit int) []RouteIssue {
	if offset >= len(all) {
		return []RouteIssue{}
	}
	end := min(offset+limit, len(all))
	out := append([]RouteIssue(nil), all[offset:end]...)
	for i := range out {
		if len(out[i].Reason) > 1024 {
			out[i].Reason = boundedText(out[i].Reason, 1024)
			out[i].ReasonTruncated = true
		}
	}
	return out
}

type RouteIssuePage struct {
	Total      int          `json:"total"`
	Offset     int          `json:"offset"`
	NextOffset *int         `json:"next_offset,omitempty"`
	Issues     []RouteIssue `json:"issues"`
}

func (r *Runtime) observationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		name := req.PathValue("name")
		b, ok := r.bindings[name]
		var out InstanceStatus
		if ok {
			out = r.instanceStatusLocked(name, b, r.records[name], true)
		}
		r.mu.Unlock()
		if !ok {
			localapi.Fail(w, http.StatusNotFound, errors.New("unknown service instance"))
			return
		}
		localapi.JSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/services/route-issues", func(w http.ResponseWriter, req *http.Request) {
		offset, err := strconv.Atoi(req.URL.Query().Get("offset"))
		if req.URL.Query().Get("offset") == "" {
			offset, err = 0, nil
		}
		if err != nil || offset < 0 {
			localapi.Fail(w, http.StatusBadRequest, errors.New("route issue offset must be a nonnegative integer"))
			return
		}
		r.mu.Lock()
		page := RouteIssuePage{Total: len(r.routeIssues), Offset: offset, Issues: boundedIssues(r.routeIssues, offset, statusRouteLimit)}
		if offset < page.Total && page.Total-offset > len(page.Issues) {
			next := offset + len(page.Issues)
			page.NextOffset = &next
		}
		r.mu.Unlock()
		localapi.JSON(w, http.StatusOK, page)
	})
}
