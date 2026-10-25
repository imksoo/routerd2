package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/eventcontroller"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

// ConfirmationMetadata identifies a pending confirmed commit without copying
// its complete rollback document into every health check and acknowledgment.
type ConfirmationMetadata struct {
	PreviousRevision uint64    `json:"previous_revision"`
	Deadline         time.Time `json:"deadline"`
	RequestID        string    `json:"request_id"`
}

// Health preserves the field names consumed by releaseboot and old health
// clients, while containing no configuration or protocol snapshot bodies.
type Health struct {
	StoreID            string                    `json:"store_id"`
	Revision           uint64                    `json:"revision"`
	Healthy            bool                      `json:"healthy"`
	LastError          string                    `json:"last_error,omitempty"`
	LastErrorTruncated bool                      `json:"last_error_truncated,omitempty"`
	ProducerEpoch      string                    `json:"producer_epoch"`
	Sequence           uint64                    `json:"sequence"`
	Confirmation       *ConfirmationMetadata     `json:"confirmation,omitempty"`
	Services           *runtimeplan.HealthStatus `json:"services,omitempty"`
	EventJournal       JournalHealth             `json:"event_journal"`
	EventRules         RuleHealth                `json:"event_rules"`
	Derived            DerivedHealth             `json:"derived"`
	ProtocolEvents     ProtocolHealth            `json:"protocol_events"`
}

type ProtocolHealth struct {
	protocolevents.RetirementStatus
	LastError string `json:"last_error,omitempty"`
}

type DerivedHealth struct {
	derivedcontroller.Health
	LastError string `json:"last_error,omitempty"`
}

type RuleHealth struct {
	eventcontroller.Status
	LastError string `json:"last_error,omitempty"`
}

type JournalHealth struct {
	Pending   int              `json:"pending"`
	LastError string           `json:"last_error,omitempty"`
	Storage   EventStoreHealth `json:"storage"`
}

// ConfigurationMetadata is also the bounded success response for a commit or
// confirmation whose old full-State response would exceed the API bound.
// Its fields decode into State; omitted documents are always explicit.
type ConfigurationMetadata struct {
	Health
	LastRequestID               string `json:"last_request_id,omitempty"`
	LastRequestHash             string `json:"last_request_hash,omitempty"`
	ConfigOmitted               bool   `json:"config_omitted"`
	ConfirmationPreviousOmitted bool   `json:"confirmation_previous_omitted,omitempty"`
	ServicesOmitted             bool   `json:"services_omitted,omitempty"`
	ConfigDigest                string `json:"config_digest"`
	ConfigBytes                 int    `json:"config_bytes"`
}

func stateText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (e *Engine) healthForState(s State) Health {
	h := Health{StoreID: s.StoreID, Revision: s.Revision, Healthy: s.Healthy, LastError: stateText(s.LastError, 4096), LastErrorTruncated: len(s.LastError) > 4096, ProducerEpoch: s.ProducerEpoch, Sequence: s.Sequence}
	h.EventJournal = JournalHealth{Pending: len(s.EventOutbox), LastError: s.EventJournalError}
	h.EventRules.LastError = s.EventRuleError
	h.Derived.LastError = s.DerivedError
	h.ProtocolEvents.LastError = s.ProtocolEventError
	e.mu.Lock()
	h.EventJournal.Storage = e.eventStore
	if e.eventStore.LastError != "" {
		h.Healthy = false
	}
	if e.protocolRelays != nil {
		h.ProtocolEvents.RetirementStatus = e.protocolRelays.Status()
	}
	if e.rules != nil {
		h.EventRules.Status = e.rules.Status()
		if h.EventRules.Revision != s.Revision {
			h.Healthy = false
		}
	}
	if e.derived != nil {
		h.Derived.Health = e.derived.Health()
		if h.Derived.Revision != s.Revision {
			h.Healthy = false
		}
	}
	e.mu.Unlock()
	if s.ProtocolEventError != "" {
		h.Healthy = false
	}
	if s.DerivedError != "" {
		h.Healthy = false
	}
	if s.EventRuleError != "" {
		h.Healthy = false
	}
	if s.EventJournalError != "" {
		h.Healthy = false
	}
	if s.EventStoreError != "" {
		h.Healthy = false
	}
	if c := s.Confirmation; c != nil {
		h.Confirmation = &ConfirmationMetadata{PreviousRevision: c.PreviousRevision, Deadline: c.Deadline, RequestID: c.RequestID}
	}
	if e.runtime != nil {
		services := e.runtime.Health()
		h.Services = &services
		if services.Phase != "ready" || services.Revision != s.Revision {
			h.Healthy = false
			if services.LastError != "" {
				h.LastError = services.LastError
				h.LastErrorTruncated = services.LastErrorTruncated
			}
		}
	}
	return h
}

func (e *Engine) health() Health {
	e.mu.Lock()
	s := e.state
	e.mu.Unlock()
	return e.healthForState(s)
}

func (e *Engine) metadataForState(s State) (ConfigurationMetadata, error) {
	encoded, err := json.Marshal(s.Config)
	if err != nil {
		return ConfigurationMetadata{}, err
	}
	digest := sha256.Sum256(encoded)
	h := e.healthForState(s)
	return ConfigurationMetadata{Health: h, LastRequestID: s.LastRequestID, LastRequestHash: s.LastRequestHash, ConfigOmitted: true, ConfirmationPreviousOmitted: s.Confirmation != nil, ServicesOmitted: s.Services != nil || h.Services != nil, ConfigDigest: hex.EncodeToString(digest[:]), ConfigBytes: len(encoded)}, nil
}

func writeStateJSON(w http.ResponseWriter, code int, encoded []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(encoded)
}

func (e *Engine) configurationViewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/config/metadata", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		s := e.state
		e.mu.Unlock()
		metadata, err := e.metadataForState(s)
		if err != nil {
			localapi.Fail(w, http.StatusInternalServerError, err)
			return
		}
		localapi.JSON(w, http.StatusOK, metadata)
	})
	mux.HandleFunc("GET /v1/config/document", func(w http.ResponseWriter, r *http.Request) {
		values, queryErr := url.ParseQuery(r.URL.RawQuery)
		revision, err := strconv.ParseUint(values.Get("revision"), 10, 64)
		if queryErr != nil || err != nil || revision == 0 || len(values) != 1 || len(values["revision"]) != 1 {
			localapi.Fail(w, http.StatusBadRequest, errors.New("revision is required and must be a single positive integer"))
			return
		}
		e.mu.Lock()
		s := e.state
		e.mu.Unlock()
		if s.Revision != revision {
			localapi.Fail(w, http.StatusConflict, ErrConflict)
			return
		}
		encoded, err := json.Marshal(s.Config)
		if err != nil || len(encoded) > localapi.MaxBody {
			localapi.Fail(w, http.StatusInternalServerError, errors.New("committed configuration cannot be represented within the document bound"))
			return
		}
		// Exactly one compact document, without an extra response envelope or
		// newline. Metadata's revision/digest fences this independently read body.
		writeStateJSON(w, http.StatusOK, encoded)
	})
}

func (e *Engine) writeConfigurationState(w http.ResponseWriter, s State, mutation bool) {
	if s.EventJournalError != "" || s.EventStoreError != "" || s.EventRuleError != "" || s.DerivedError != "" || s.ProtocolEventError != "" {
		s.Healthy = false
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		localapi.Fail(w, http.StatusInternalServerError, err)
		return
	}
	if len(encoded) <= localapi.MaxBody {
		writeStateJSON(w, http.StatusOK, encoded)
		return
	}
	if !mutation {
		localapi.Fail(w, http.StatusRequestEntityTooLarge, errors.New("configuration state exceeds the response bound; use /v1/config/metadata and /v1/config/document?revision=N"))
		return
	}
	metadata, err := e.metadataForState(s)
	if err != nil {
		localapi.Fail(w, http.StatusInternalServerError, err)
		return
	}
	localapi.JSON(w, http.StatusOK, metadata)
}
