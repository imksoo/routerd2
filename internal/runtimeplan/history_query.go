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
				return since, until, limit, errors.New("日時は保存可能な範囲のRFC3339形式で指定してください")
			}
			*field.value = value
		}
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return since, until, limit, errors.New("終了日時には開始日時以降を指定してください")
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			return since, until, limit, errors.New("表示上限は1〜1000の整数で指定してください")
		}
	}
	return since, until, limit, nil
}
