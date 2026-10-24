package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
)

type networkObserver interface {
	Observe(context.Context) (network.ObservationView, error)
}

type networkDetailObserver interface {
	ObservationPage(context.Context, network.ObservationQuery) (network.ObservationPage, error)
	DesiredObservation(context.Context, string, int, int) (network.DesiredPage, error)
}

type networkSourceObserver interface {
	SourceAddress(context.Context, string, string) (network.SourceObservation, error)
}

func networkObservation(ctx context.Context, backend interface {
	Snapshot(context.Context) (network.Snapshot, error)
}) (network.ObservationView, error) {
	if observer, ok := backend.(networkObserver); ok {
		return observer.Observe(ctx)
	}
	// Small in-process adapters can retain their old Snapshot contract. The
	// native worker and NetworkClient always use the explicit bounded view.
	s, err := backend.Snapshot(ctx)
	if err != nil {
		return network.ObservationView{}, err
	}
	return network.BoundSnapshot(s)
}

func observationFailure(w http.ResponseWriter, err error) {
	code := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, network.ErrObservationChanged), localapi.IsHTTPStatus(err, http.StatusConflict):
		code = http.StatusConflict
	case errors.Is(err, network.ErrObservationNotFound), localapi.IsHTTPStatus(err, http.StatusNotFound):
		code = http.StatusNotFound
	case errors.Is(err, network.ErrObservationTooLarge), localapi.IsHTTPStatus(err, http.StatusRequestEntityTooLarge):
		code = http.StatusRequestEntityTooLarge
	case localapi.IsHTTPStatus(err, http.StatusBadRequest):
		code = http.StatusBadRequest
	}
	localapi.Fail(w, code, errors.New(stateText(err.Error(), 4096)))
}

func writeObservation(w http.ResponseWriter, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		observationFailure(w, err)
		return
	}
	if len(encoded) > network.ObservationResponseLimit {
		observationFailure(w, network.ErrObservationTooLarge)
		return
	}
	writeStateJSON(w, http.StatusOK, encoded)
}

func observationQueryValues(r *http.Request, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("invalid observation query encoding")
	}
	for name, entries := range values {
		known := false
		for _, key := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known || len(entries) != 1 || entries[0] == "" {
			return nil, errors.New("observation query contains an unknown, duplicate or empty parameter")
		}
	}
	return values, nil
}

func observationInteger(values url.Values, key string, minimum, maximum int) (int, error) {
	text := values.Get(key)
	if text == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(text, 10, strconv.IntSize-1)
	if err != nil || value < uint64(minimum) || value > uint64(maximum) {
		return 0, errors.New("observation " + key + " is outside its permitted range")
	}
	return int(value), nil
}

func parseObservationQuery(r *http.Request, section string) (network.ObservationQuery, error) {
	q := network.ObservationQuery{Section: section}
	values, err := observationQueryValues(r, "interface", "public_key", "route_id", "table", "offset", "limit", "projection_id")
	if err != nil {
		return q, err
	}
	q.Interface, q.PublicKey, q.RouteID, q.ProjectionID = values.Get("interface"), values.Get("public_key"), values.Get("route_id"), values.Get("projection_id")
	q.Offset, err = observationInteger(values, "offset", 0, int(^uint(0)>>1))
	if err != nil {
		return q, err
	}
	q.Limit, err = observationInteger(values, "limit", 1, network.ObservationPageLimit)
	if err != nil {
		return q, err
	}
	q.Table, err = observationInteger(values, "table", 0, int(^uint32(0)))
	if err != nil {
		return q, err
	}
	return q, q.Validate()
}

func networkObservationRoutes(mux *http.ServeMux, prefix string, backend any) {
	mux.HandleFunc("GET "+prefix+"{section}", func(w http.ResponseWriter, r *http.Request) {
		section := r.PathValue("section")
		observer, ok := backend.(networkDetailObserver)
		if !ok {
			observationFailure(w, errors.New("native network detail observation is unavailable"))
			return
		}
		if section == "desired" {
			values, err := observationQueryValues(r, "offset", "limit", "projection_id")
			var offset, limit int
			if err == nil {
				offset, err = observationInteger(values, "offset", 0, network.MaxDesiredObservationBytes)
			}
			if err == nil {
				limit, err = observationInteger(values, "limit", 1, network.DesiredObservationChunk)
			}
			if err == nil {
				err = network.ValidateDesiredObservationQuery(values.Get("projection_id"), offset, limit)
			}
			if err != nil {
				localapi.Fail(w, http.StatusBadRequest, err)
				return
			}
			page, err := observer.DesiredObservation(r.Context(), values.Get("projection_id"), offset, limit)
			if err != nil {
				observationFailure(w, err)
				return
			}
			writeObservation(w, page)
			return
		}
		q, err := parseObservationQuery(r, section)
		if err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		page, err := observer.ObservationPage(r.Context(), q)
		if err != nil {
			observationFailure(w, err)
			return
		}
		writeObservation(w, page)
	})
}

func networkSourceRoute(mux *http.ServeMux, backend any) {
	mux.HandleFunc("GET /v1/source", func(w http.ResponseWriter, r *http.Request) {
		values, err := observationQueryValues(r, "interface", "address")
		if err == nil {
			err = network.ValidateSourceAddressQuery(values.Get("interface"), values.Get("address"))
		}
		if err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		observer, ok := backend.(networkSourceObserver)
		if !ok {
			observationFailure(w, errors.New("native source address observation is unavailable"))
			return
		}
		value, err := observer.SourceAddress(r.Context(), values.Get("interface"), values.Get("address"))
		if err != nil {
			observationFailure(w, err)
			return
		}
		writeObservation(w, value)
	})
}

func (n *NetworkClient) Observe(ctx context.Context) (value network.ObservationView, err error) {
	err = n.client.Do(ctx, http.MethodGet, "/v1/snapshot", nil, &value)
	return
}

func (n *NetworkClient) ObservationPage(ctx context.Context, q network.ObservationQuery) (value network.ObservationPage, err error) {
	if err = q.Validate(); err != nil {
		return
	}
	query := url.Values{}
	for key, value := range map[string]string{"interface": q.Interface, "public_key": q.PublicKey, "route_id": q.RouteID, "projection_id": q.ProjectionID} {
		if value != "" {
			query.Set(key, value)
		}
	}
	for key, value := range map[string]int{"table": q.Table, "offset": q.Offset, "limit": q.Limit} {
		if value != 0 {
			query.Set(key, strconv.Itoa(value))
		}
	}
	err = n.client.Do(ctx, http.MethodGet, "/v1/observations/"+q.Section+"?"+query.Encode(), nil, &value)
	return
}

func (n *NetworkClient) DesiredObservation(ctx context.Context, projectionID string, offset, limit int) (value network.DesiredPage, err error) {
	if err = network.ValidateDesiredObservationQuery(projectionID, offset, limit); err != nil {
		return
	}
	query := url.Values{"offset": {strconv.Itoa(offset)}}
	if projectionID != "" {
		query.Set("projection_id", projectionID)
	}
	if limit != 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	err = n.client.Do(ctx, http.MethodGet, "/v1/observations/desired?"+query.Encode(), nil, &value)
	return
}

func (n *NetworkClient) SourceAddress(ctx context.Context, name, address string) (value network.SourceObservation, err error) {
	if err = network.ValidateSourceAddressQuery(name, address); err != nil {
		return
	}
	query := url.Values{"interface": {name}, "address": {address}}
	err = n.client.Do(ctx, http.MethodGet, "/v1/source?"+query.Encode(), nil, &value)
	return
}
