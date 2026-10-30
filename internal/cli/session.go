// Package cli is a typed management shell shared by local and SSH entry points.
// It deliberately has no escape to a POSIX shell, execve, forwarding, or SFTP.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/supervisor"
)

type Session struct{ RuntimeDir string }

func WriteJSON(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func (s Session) Execute(ctx context.Context, command string, input io.Reader, out io.Writer) error {
	a := strings.Fields(command)
	if len(a) == 0 {
		return nil
	}
	if len(a) == 1 && a[0] == "help" {
		_, err := fmt.Fprintln(out, "show status\nshow network [SECTION [FILTER VALUE ...]] (show network help for details)\nshow resources\nshow derived RESOURCE_ID\nshow observations [after CURSOR] [limit 1..16]\nshow event-store\nshow services [NAME]\nshow route-issues [offset N]\nshow config\nshow snapshots\nshow running-config\nshow logs [module NAME] [level INFO] [limit 100]\nshow dns-queries NAME [client IP] [name PATTERN] [response RCODE] [since RFC3339] [until RFC3339] [limit 1..1000]\nshow logging\nshow version\nrestart <module>\npoweroff\nreboot\nconfigure (exec: CommitRequest YAML/JSON on stdin; shell: candidate mode)\nconfirm\nsnapshot (exec stdin: SnapshotRequest JSON/YAML)\nrestore (exec stdin: RestoreRequest JSON/YAML)\nupdate status|stage|prepare|rollback|cancel (exec stdin: signed tar or JSON)\nhelp\nexit")
		return err
	}
	var result json.RawMessage
	super := localapi.NewClient(filepath.Join(s.RuntimeDir, "supervisor.sock"))
	ctl := localapi.NewClient(filepath.Join(s.RuntimeDir, control.SocketName))
	logClient := localapi.NewClient(filepath.Join(s.RuntimeDir, logging.SocketName))
	var err error
	switch {
	case len(a) == 1 && a[0] == "snapshot":
		return snapshotConfiguration(ctx, ctl, input, out)
	case len(a) == 1 && a[0] == "restore":
		if input == nil {
			return errors.New("restore requires RestoreRequest JSON/YAML on stdin")
		}
		var request control.RestoreRequest
		if err := config.Decode(input, &request); err != nil {
			return err
		}
		state, err := s.RestoreConfiguration(ctx, request)
		if err != nil {
			return fmt.Errorf("request_id=%s: %w", request.RequestID, err)
		}
		return WriteConfigurationState(out, state)
	case a[0] == "update":
		return update(ctx, ctl, a[1:], input, out)
	case len(a) >= 2 && a[0] == "show" && a[1] == "event-store":
		return showEventStore(ctx, ctl, a[2:], out)
	case len(a) >= 2 && a[0] == "show" && a[1] == "derived":
		return showDerived(ctx, ctl, a[2:], out)
	case len(a) >= 2 && a[0] == "show" && a[1] == "observations":
		return showObservations(ctx, ctl, a[2:], out)
	case len(a) == 2 && a[0] == "show" && a[1] == "resources":
		return showResources(ctx, ctl, out)
	case len(a) >= 2 && a[0] == "show" && a[1] == "network":
		return showNetwork(ctx, ctl, a[2:], out)
	case len(a) >= 2 && a[0] == "show" && a[1] == "dns-queries":
		return showDNSHistory(ctx, ctl, a[2:], out)
	case len(a) >= 2 && a[0] == "show" && a[1] == "logs":
		q := url.Values{"tail": {"true"}, "limit": {"100"}}
		if len(a)%2 != 0 {
			return errors.New("usage: show logs [module NAME] [level LEVEL] [limit 1..200]")
		}
		seen := map[string]bool{}
		for i := 2; i < len(a); i += 2 {
			if seen[a[i]] {
				return errors.New("duplicate log filter")
			}
			seen[a[i]] = true
			switch a[i] {
			case "module":
				q.Set("module", a[i+1])
			case "level":
				q.Set("level", strings.ToUpper(a[i+1]))
			case "limit":
				n, e := strconv.Atoi(a[i+1])
				if e != nil || n < 1 || n > logging.MaxQuery {
					return errors.New("log limit must be 1..200")
				}
				q.Set("limit", a[i+1])
			default:
				return errors.New("unknown log filter")
			}
		}
		err = logClient.Do(ctx, http.MethodGet, "/v1/logs?"+q.Encode(), nil, &result)
	case len(a) == 3 && a[0] == "show" && a[1] == "services":
		name := a[2]
		if strings.ContainsAny(name, "/\\?%") || len(name) > 100 {
			return errors.New("invalid service name")
		}
		if !strings.HasPrefix(name, "service.") {
			name = "service." + name
		}
		err = ctl.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(name), nil, &result)
	case len(a) >= 2 && a[0] == "show" && a[1] == "route-issues":
		offset := 0
		if len(a) == 4 && a[2] == "offset" {
			offset, err = strconv.Atoi(a[3])
		} else if len(a) != 2 {
			return errors.New("usage: show route-issues [offset N]")
		}
		if err != nil || offset < 0 {
			return errors.New("route issue offset must be a nonnegative integer")
		}
		err = ctl.Do(ctx, http.MethodGet, "/v1/services/route-issues?offset="+strconv.Itoa(offset), nil, &result)
	case len(a) == 2 && a[0] == "show":
		switch a[1] {
		case "status":
			err = super.Do(ctx, http.MethodGet, "/v1/status", nil, &result)
		case "services":
			err = ctl.Do(ctx, http.MethodGet, "/v1/services", nil, &result)
		case "logging":
			err = logClient.Do(ctx, http.MethodGet, "/v1/status", nil, &result)
		case "snapshots":
			var listing config.SnapshotList
			if err := ctl.Do(ctx, http.MethodGet, "/v1/config/snapshots", nil, &listing); err != nil {
				return err
			}
			return WriteJSON(out, listing)
		case "config":
			state, err := readConfiguration(ctx, ctl, true)
			if err != nil {
				return err
			}
			return WriteConfigurationState(out, state)
		case "running-config":
			state, err := readConfiguration(ctx, ctl, true)
			if err != nil {
				return err
			}
			y, err := config.YAML(state.Config)
			if err != nil {
				return err
			}
			_, err = out.Write(y)
			return err
		case "version":
			return WriteJSON(out, buildinfo.Current())
		default:
			return errors.New("unknown show command; use help")
		}
	case len(a) == 1 && (a[0] == "poweroff" || a[0] == "reboot"):
		err = super.Do(ctx, http.MethodPost, "/v1/power", supervisor.PowerRequest{Action: supervisor.PowerAction(a[0])}, &result)
	case len(a) == 2 && a[0] == "restart":
		if strings.ContainsAny(a[1], "/\\?") {
			return errors.New("invalid module name")
		}
		if strings.HasPrefix(a[1], "service.") {
			err = ctl.Do(ctx, http.MethodPost, "/v1/services/"+url.PathEscape(a[1])+"/restart", nil, &result)
		} else {
			err = super.Do(ctx, http.MethodPost, "/v1/modules/"+url.PathEscape(a[1])+"/restart", struct{}{}, &result)
		}
	case len(a) == 1 && a[0] == "configure":
		if input == nil {
			return errors.New("configure requires an exec request with JSON on stdin")
		}
		var r control.CommitRequest
		if err := config.Decode(input, &r); err != nil {
			return err
		}
		state, err := commitConfiguration(ctx, ctl, r)
		if err != nil {
			return fmt.Errorf("request_id=%s: %w", r.RequestID, err)
		}
		return WriteConfigurationState(out, state)
	case len(a) == 1 && a[0] == "confirm":
		state, err := readConfiguration(ctx, ctl, false)
		if err != nil {
			return err
		}
		if state.Confirmation == nil {
			return errors.New("no revision is awaiting confirmation")
		}
		confirmed, err := confirmConfiguration(ctx, ctl, control.ConfirmRequest{Revision: state.Revision, RequestID: state.Confirmation.RequestID})
		if err != nil {
			return err
		}
		return WriteConfigurationState(out, confirmed)
	default:
		return errors.New("unknown command; use help")
	}
	if err != nil {
		return err
	}
	return WriteJSON(out, result)
}
