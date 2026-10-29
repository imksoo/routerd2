package runtimeplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

const resourceStatusPageSize = 128

// ResourceObservation reports the coordinator's last selected projection.
// Ready is the policy condition value, not independent kernel readback. Phase
// and LastError expose pending or failed reconciliation. No source payloads,
// provenance, credentials or worker snapshot bodies are copied here.
type ResourceObservation struct {
	ID    string                        `json:"id"`
	State resourceprogram.ResourceState `json:"state"`
}

type ResourceObservationPage struct {
	Revision             uint64                `json:"revision"`
	ProjectionGeneration uint64                `json:"projection_generation"`
	Phase                string                `json:"phase"`
	LastError            string                `json:"last_error,omitempty"`
	LastErrorTruncated   bool                  `json:"last_error_truncated,omitempty"`
	ObservationID        string                `json:"observation_id"`
	Total                int                   `json:"total"`
	Offset               int                   `json:"offset"`
	NextOffset           *int                  `json:"next_offset,omitempty"`
	Resources            []ResourceObservation `json:"resources"`
}

func (r *Runtime) resourceObservation(offset int, expected string) (ResourceObservationPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	phase, lastError := r.phaseLocked()
	page := ResourceObservationPage{Revision: r.config.revision, ProjectionGeneration: r.projectionGeneration, Phase: phase, LastError: boundedText(lastError, 4096), LastErrorTruncated: len(lastError) > 4096, Total: len(r.resourceStates), Offset: offset}
	all := make([]ResourceObservation, 0, len(r.resourceStates))
	for id, state := range r.resourceStates {
		all = append(all, ResourceObservation{ID: id, State: state})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	// Include readiness/reasons and runtime phase: those can change even when
	// source revision and the network projection generation stay the same.
	encoded, err := json.Marshal(struct {
		Revision, Generation uint64
		Phase, LastError     string
		ErrorTruncated       bool
		Resources            []ResourceObservation
	}{page.Revision, page.ProjectionGeneration, page.Phase, page.LastError, page.LastErrorTruncated, all})
	if err != nil {
		return page, err
	}
	digest := sha256.Sum256(encoded)
	page.ObservationID = hex.EncodeToString(digest[:])
	if expected != "" && expected != page.ObservationID {
		return page, errors.New("resource observation changed; restart at offset 0")
	}
	page.Resources = []ResourceObservation{}
	if offset < len(all) {
		end := min(offset+resourceStatusPageSize, len(all))
		page.Resources = all[offset:end]
		if end < len(all) {
			page.NextOffset = &end
		}
	}
	return page, nil
}

func (r *Runtime) resourceObservationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/resources", func(w http.ResponseWriter, req *http.Request) {
		values, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			localapi.Fail(w, 400, errors.New("invalid resource query"))
			return
		}
		for key, values := range values {
			if len(values) != 1 || key != "offset" && key != "observation_id" {
				localapi.Fail(w, 400, errors.New("unknown or repeated resource query parameter"))
				return
			}
		}
		offset := 0
		if raw, present := values["offset"]; present {
			offset, err = strconv.Atoi(raw[0])
		}
		pin := values.Get("observation_id")
		decoded, pinErr := hex.DecodeString(pin)
		_, hasPin := values["observation_id"]
		if err != nil || offset < 0 || offset > 0 && pin == "" || hasPin && (pinErr != nil || len(decoded) != sha256.Size) {
			localapi.Fail(w, 400, errors.New("resource offset must be nonnegative; continuations require a 64-digit observation_id"))
			return
		}
		page, err := r.resourceObservation(offset, pin)
		if err != nil {
			localapi.Fail(w, 409, err)
			return
		}
		localapi.JSON(w, 200, page)
	})
}
