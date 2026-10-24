package control

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
)

type ObservationPage struct {
	Records    []events.Record `json:"records"`
	NextCursor uint64          `json:"next_cursor"`
	More       bool            `json:"more"`
}

func (e *Engine) observationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/observations", func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		after, cursorErr := strconv.ParseUint(query.Get("after_cursor"), 10, 64)
		limit := 16
		valid := err == nil && cursorErr == nil && after <= math.MaxInt64 && len(query["after_cursor"]) == 1
		for key, values := range query {
			if len(values) != 1 || key != "after_cursor" && key != "limit" {
				valid = false
			}
		}
		if raw, ok := query["limit"]; ok && len(raw) == 1 {
			limit, err = strconv.Atoi(raw[0])
			valid = valid && err == nil
		}
		if !valid || limit < 1 || limit > 16 {
			localapi.Fail(w, http.StatusBadRequest, errors.New("after_cursor is required; limit must be 1..16; duplicate/unknown query fields are forbidden"))
			return
		}
		if e.journal == nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("durable event journal is unavailable"))
			return
		}
		rows, err := e.journal.List(r.Context(), after, limit+1)
		if err != nil {
			code := http.StatusServiceUnavailable
			if errors.Is(err, events.ErrConflict) {
				code = http.StatusConflict
			}
			localapi.Fail(w, code, err)
			return
		}
		page := ObservationPage{Records: rows, NextCursor: after, More: len(rows) > limit}
		if page.More {
			page.Records = page.Records[:limit]
		}
		if len(page.Records) > 0 {
			page.NextCursor = page.Records[len(page.Records)-1].Cursor
		}
		// At most sixteen 64 KiB envelopes plus framing, below MaxBody.
		localapi.JSON(w, http.StatusOK, page)
	})
}
