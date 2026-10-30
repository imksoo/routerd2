// SPDX-License-Identifier: BSD-3-Clause

package runtimeplan

import (
	"errors"
	"net/url"
	"strconv"
	"time"
)

// Both historical stores use nanosecond timestamps and bounded result sets.
func historyRange(q url.Values) (since, until time.Time, limit int, err error) {
	limit = 100
	for _, field := range []struct {
		name  string
		value *time.Time
	}{{"since", &since}, {"until", &until}} {
		if raw := q.Get(field.name); raw != "" {
			value, parseErr := time.Parse(time.RFC3339Nano, raw)
			if parseErr != nil || !time.Unix(0, value.UnixNano()).Equal(value) {
				return since, until, limit, errors.New("time must be an RFC3339 timestamp within the stored range")
			}
			*field.value = value
		}
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return since, until, limit, errors.New("until must not precede since")
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			return since, until, limit, errors.New("limit must be between 1 and 1000")
		}
	}
	return since, until, limit, nil
}
