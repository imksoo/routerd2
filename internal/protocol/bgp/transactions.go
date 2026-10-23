// SPDX-License-Identifier: BSD-3-Clause
package bgp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	gobgpapi "github.com/osrg/gobgp/v4/api"
)

// A transaction error after a live mutation or withdrawal fence stops the
// worker. Merely returning HTTP 500 could leave an unjournaled advertisement
// alive forever; process restart restores only the durable, fenced journal.
type pathTransactionError struct{ err error }

func (e *pathTransactionError) Error() string { return e.err.Error() }
func (e *pathTransactionError) Unwrap() error { return e.err }

func upsertDynamicPath(ctx context.Context, server pathServer, statePath string, path bgpdaemon.AppliedPath) (bgpdaemon.AppliedConfig, *bgpdaemon.AppliedPath, error) {
	applied, ok, err := readApplied(statePath)
	if err != nil {
		return bgpdaemon.AppliedConfig{}, nil, err
	}
	if !ok {
		return bgpdaemon.AppliedConfig{}, nil, errors.New("applied BGP config is not initialized")
	}
	if err := bgpdaemon.Validate(applied); err != nil {
		return bgpdaemon.AppliedConfig{}, nil, err
	}
	path, err = validateDynamicMobilityPath(path)
	if err != nil {
		return bgpdaemon.AppliedConfig{}, nil, err
	}
	reqPath, err := pathFromAppliedPath(path)
	if err != nil {
		return bgpdaemon.AppliedConfig{}, nil, err
	}
	key := bgpdaemon.AppliedPathKey(path)
	withdrew := false
	for i, existing := range applied.Paths {
		if bgpdaemon.AppliedPathKey(existing) != key {
			continue
		}
		if reflect.DeepEqual(existing.Attrs, path.Attrs) && existing.UUID != "" {
			return applied, &applied.Paths[i], nil
		}
		// Journal the removal before touching GoBGP. A crash in the following
		// replacement window must not resurrect the obsolete mobility owner.
		applied.Paths = append(applied.Paths[:i], applied.Paths[i+1:]...)
		if err := writeApplied(statePath, applied); err != nil {
			return bgpdaemon.AppliedConfig{}, nil, err
		}
		withdrew = true
		if id, err := bgpdaemon.DecodeUUID(existing.UUID); err != nil {
			return bgpdaemon.AppliedConfig{}, nil, &pathTransactionError{err}
		} else if len(id) > 0 {
			if err := server.DeletePath(ctx, &gobgpapi.DeletePathRequest{TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL, Uuid: id}); err != nil && !isMissingGoBGPPath(err) {
				return bgpdaemon.AppliedConfig{}, nil, &pathTransactionError{err}
			}
		}
		break
	}
	resp, err := server.AddPath(ctx, &gobgpapi.AddPathRequest{TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL, Path: reqPath})
	if err != nil {
		if withdrew {
			err = &pathTransactionError{err}
		}
		return bgpdaemon.AppliedConfig{}, nil, err
	}
	path.UUID = bgpdaemon.EncodeUUID(resp.GetUuid())
	applied.Paths = append(applied.Paths, path)
	applied = bgpdaemon.Normalize(applied)
	if err := writeApplied(statePath, applied); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		removeErr := server.DeletePath(cleanup, &gobgpapi.DeletePathRequest{TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL, Uuid: resp.GetUuid()})
		return bgpdaemon.AppliedConfig{}, nil, &pathTransactionError{errors.Join(err, removeErr)}
	}
	if !applied.PendingImportPolicyReset {
		if err := refreshDynamicPathPolicies(ctx, server, applied); err != nil {
			return bgpdaemon.AppliedConfig{}, nil, &pathTransactionError{err}
		}
	}
	for i := range applied.Paths {
		if bgpdaemon.AppliedPathKey(applied.Paths[i]) == key {
			return applied, &applied.Paths[i], nil
		}
	}
	return applied, nil, nil
}

func deleteDynamicPath(ctx context.Context, server pathServer, statePath string, path bgpdaemon.AppliedPath) (bgpdaemon.AppliedConfig, error) {
	applied, ok, err := readApplied(statePath)
	if err != nil {
		return bgpdaemon.AppliedConfig{}, err
	}
	if !ok {
		return bgpdaemon.AppliedConfig{}, errors.New("applied BGP config is not initialized")
	}
	if err := bgpdaemon.Validate(applied); err != nil {
		return bgpdaemon.AppliedConfig{}, err
	}
	path, err = validateDynamicMobilityPath(path)
	if err != nil {
		return bgpdaemon.AppliedConfig{}, err
	}
	key := bgpdaemon.AppliedPathKey(path)
	for i, existing := range applied.Paths {
		if bgpdaemon.AppliedPathKey(existing) != key {
			continue
		}
		applied.Paths = append(applied.Paths[:i], applied.Paths[i+1:]...)
		applied = bgpdaemon.Normalize(applied)
		if err := writeApplied(statePath, applied); err != nil {
			return bgpdaemon.AppliedConfig{}, err
		}
		if id, err := bgpdaemon.DecodeUUID(existing.UUID); err != nil {
			return bgpdaemon.AppliedConfig{}, &pathTransactionError{err}
		} else if len(id) > 0 {
			if err := server.DeletePath(ctx, &gobgpapi.DeletePathRequest{TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL, Uuid: id}); err != nil && !isMissingGoBGPPath(err) {
				return bgpdaemon.AppliedConfig{}, &pathTransactionError{err}
			}
		}
		if !applied.PendingImportPolicyReset {
			if err := refreshDynamicPathPolicies(ctx, server, applied); err != nil {
				return bgpdaemon.AppliedConfig{}, &pathTransactionError{err}
			}
		}
		return applied, nil
	}
	return applied, nil
}

func signalTransactionFailure(monitor *observation, err error) {
	var fatal *pathTransactionError
	if monitor != nil && monitor.fail != nil && errors.As(err, &fatal) {
		monitor.fail(fmt.Errorf("BGP path transaction requires restart: %w", err))
	}
}
