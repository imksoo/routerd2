package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
)

const networkHelp = `show network
show network interfaces [offset N] [limit 1..256] [projection ID]
show network addresses interface IF [offset N] [limit 1..256] [projection ID]
show network routes [table N] [offset N] [limit 1..256] [projection ID]
show network route-next-hops route-id ID [table N] [offset N] [limit 1..256] [projection ID]
show network policy-rules [offset N] [limit 1..256] [projection ID]
show network wireguard [offset N] [limit 1..256] [projection ID]
show network wireguard-peers interface IF [offset N] [limit 1..256] [projection ID]
show network wireguard-allowed-ips interface IF public-key KEY [offset N] [limit 1..256] [projection ID]
show network desired [projection ID]
Detail pages default to 64 items; next_command continues the same projection.
Projection IDs detect configuration changes. Kernel observations remain live:
addresses, counters and other runtime state can change between pages.
`

func networkFilters(section string) []string {
	var filters []string
	switch section {
	case "interfaces", "policy-rules", "wireguard":
	case "addresses", "wireguard-peers":
		filters = []string{"interface"}
	case "wireguard-allowed-ips":
		filters = []string{"interface", "public-key"}
	case "routes":
		filters = []string{"table"}
	case "route-next-hops":
		filters = []string{"route-id", "table"}
	case "desired":
		return []string{"projection"}
	default:
		return nil
	}
	return append(filters, "offset", "limit", "projection")
}

func parseNetworkQuery(args []string) (network.ObservationQuery, error) {
	if len(args) == 0 || len(args)%2 != 1 {
		return network.ObservationQuery{}, errors.New("usage: show network SECTION [FILTER VALUE ...]; use show network help")
	}
	query := network.ObservationQuery{Section: args[0], Limit: 64}
	allowed := map[string]bool{}
	for _, name := range networkFilters(query.Section) {
		allowed[name] = true
	}
	if len(allowed) == 0 {
		return query, errors.New("unknown network section; use show network help")
	}
	seen := map[string]bool{}
	for i := 1; i < len(args); i += 2 {
		name, value := args[i], args[i+1]
		if !allowed[name] || seen[name] {
			return query, fmt.Errorf("unknown, inapplicable or duplicate network filter %q", name)
		}
		seen[name] = true
		switch name {
		case "interface":
			query.Interface = value
		case "public-key":
			query.PublicKey = value
		case "route-id":
			query.RouteID = value
		case "projection":
			query.ProjectionID = value
		case "offset", "limit", "table":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || name == "limit" && n == 0 {
				return query, fmt.Errorf("network %s must be a nonnegative integer (limit must be positive)", name)
			}
			switch name {
			case "offset":
				query.Offset = n
			case "limit":
				query.Limit = n
			case "table":
				query.Table = n
			}
		}
	}
	if query.Section == "desired" {
		return query, network.ValidateDesiredObservationQuery(query.ProjectionID, 0, network.DesiredObservationChunk)
	}
	return query, query.Validate()
}

func networkQueryValues(q network.ObservationQuery) url.Values {
	v := url.Values{"offset": {strconv.Itoa(q.Offset)}, "limit": {strconv.Itoa(q.Limit)}}
	for name, value := range map[string]string{"interface": q.Interface, "public_key": q.PublicKey, "route_id": q.RouteID, "projection_id": q.ProjectionID} {
		if value != "" {
			v.Set(name, value)
		}
	}
	if q.Table != 0 {
		v.Set("table", strconv.Itoa(q.Table))
	}
	return v
}

func networkNextCommand(q network.ObservationQuery, page network.ObservationPage) string {
	if page.NextOffset == nil {
		return ""
	}
	parts := []string{"show", "network", q.Section}
	for _, pair := range [][2]string{{"interface", q.Interface}, {"public-key", q.PublicKey}, {"route-id", q.RouteID}} {
		if pair[1] != "" {
			parts = append(parts, pair[0], pair[1])
		}
	}
	if q.Table != 0 {
		parts = append(parts, "table", strconv.Itoa(q.Table))
	}
	parts = append(parts, "offset", strconv.Itoa(*page.NextOffset), "limit", strconv.Itoa(q.Limit), "projection", page.ProjectionID)
	return strings.Join(parts, " ")
}

func checkObservationHeader(header network.ObservationHeader, projection string) error {
	digest, err := hex.DecodeString(header.ProjectionID)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != header.ProjectionID || header.ObservedAt.IsZero() {
		return errors.New("network observation has an invalid projection ID or timestamp")
	}
	if projection != "" && header.ProjectionID != projection {
		return network.ErrObservationChanged
	}
	return nil
}

func checkObservationPage(q network.ObservationQuery, page network.ObservationPage) error {
	if err := checkObservationHeader(page.ObservationHeader, q.ProjectionID); err != nil {
		return err
	}
	if page.Section != q.Section || page.Offset != q.Offset || page.Total < 0 || len(page.Items) > q.Limit {
		return errors.New("network page does not match the requested section, offset or limit")
	}
	remaining := max(0, page.Total-page.Offset)
	if len(page.Items) > remaining {
		return errors.New("network page item count exceeds its reported total")
	}
	if len(page.Items) < remaining {
		if len(page.Items) == 0 || page.NextOffset == nil || *page.NextOffset != page.Offset+len(page.Items) {
			return errors.New("network page has a missing or nonprogressing continuation")
		}
	} else if page.NextOffset != nil {
		return errors.New("complete network page has an unexpected continuation")
	}
	return nil
}

func networkReadError(err error) error {
	if localapi.IsHTTPStatus(err, http.StatusConflict) {
		return fmt.Errorf("%w: %v", network.ErrObservationChanged, err)
	}
	return err
}

func showNetwork(ctx context.Context, client configurationClient, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "help" {
		_, err := io.WriteString(out, networkHelp)
		return err
	}
	if len(args) == 0 {
		var view network.ObservationView
		if err := client.Do(ctx, http.MethodGet, "/v1/network", nil, &view); err != nil {
			return err
		}
		if err := checkObservationHeader(network.ObservationHeader{ProjectionID: view.ProjectionID, ObservedAt: view.ObservedAt}, ""); err != nil {
			return err
		}
		// Preserve the public counts and flags, but never render an omitted
		// desired document's zero value as though it were the current intent.
		encoded, err := json.Marshal(view)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return err
		}
		if view.Truncation.Desired {
			delete(fields, "desired")
		}
		if view.Truncated || !view.Complete {
			fields["notice"], _ = json.Marshal("This observation is a preview or has observation errors; inspect counts, truncation and observation_errors. Use show network help for detail commands.")
		}
		return WriteJSON(out, fields)
	}
	query, err := parseNetworkQuery(args)
	if err != nil {
		return err
	}
	if query.Section == "desired" {
		return showNetworkDesired(ctx, client, query.ProjectionID, out)
	}
	var page network.ObservationPage
	if err := client.Do(ctx, http.MethodGet, "/v1/network/"+query.Section+"?"+networkQueryValues(query).Encode(), nil, &page); err != nil {
		return networkReadError(err)
	}
	if err := checkObservationPage(query, page); err != nil {
		return err
	}
	return WriteJSON(out, struct {
		network.ObservationPage
		NextCommand string `json:"next_command,omitempty"`
	}{page, networkNextCommand(query, page)})
}

func showNetworkDesired(ctx context.Context, client configurationClient, projection string, out io.Writer) error {
	var header network.ObservationHeader
	var started time.Time
	var data []byte
	total := 0
	for {
		query := url.Values{"offset": {strconv.Itoa(len(data))}, "limit": {strconv.Itoa(network.DesiredObservationChunk)}}
		if projection != "" {
			query.Set("projection_id", projection)
		}
		var page network.DesiredPage
		if err := client.Do(ctx, http.MethodGet, "/v1/network/desired?"+query.Encode(), nil, &page); err != nil {
			return networkReadError(err)
		}
		if err := checkObservationHeader(page.ObservationHeader, projection); err != nil {
			return err
		}
		if page.TotalBytes < 1 || page.TotalBytes > network.MaxDesiredObservationBytes || page.Offset != len(data) {
			return errors.New("network desired page has an invalid total size or byte offset")
		}
		if len(data) == 0 {
			header, started, total, projection = page.ObservationHeader, page.ObservedAt, page.TotalBytes, page.ProjectionID
			data = make([]byte, 0, total)
		} else if page.TotalBytes != total || page.Revision != header.Revision || page.AttemptedRevision != header.AttemptedRevision || page.Status != header.Status {
			return errors.New("network desired metadata changed between pages")
		}
		// The endpoint serves exact requested byte chunks. Require that contract
		// so one command has a finite number of requests (at most 32).
		if len(page.Data) != min(network.DesiredObservationChunk, total-len(data)) || len(page.Data) == 0 {
			return errors.New("network desired page has an invalid or nonprogressing data chunk")
		}
		data = append(data, page.Data...)
		header.ObservedAt = page.ObservedAt
		if len(data) == total {
			if page.NextOffset != nil {
				return errors.New("complete network desired document has an unexpected continuation")
			}
			break
		}
		if page.NextOffset == nil || *page.NextOffset != len(data) {
			return errors.New("network desired page has a missing or inconsistent continuation")
		}
	}
	var desired network.Desired
	if err := config.Decode(bytes.NewReader(data), &desired); err != nil {
		return fmt.Errorf("invalid network desired document: %w", err)
	}
	if err := network.Validate(desired); err != nil {
		return fmt.Errorf("invalid network desired document: %w", err)
	}
	return WriteJSON(out, struct {
		network.ObservationHeader
		StartedAt  time.Time       `json:"started_at"`
		TotalBytes int             `json:"total_bytes"`
		Desired    network.Desired `json:"desired"`
	}{header, started, total, desired})
}

func networkCompletions(words []string) []string {
	if len(words) == 2 {
		return []string{"help", "interfaces", "addresses", "routes", "route-next-hops", "policy-rules", "wireguard", "wireguard-peers", "wireguard-allowed-ips", "desired"}
	}
	if len(words)%2 == 0 {
		return nil // This position expects a filter value, not another key.
	}
	used := map[string]bool{}
	for i := 3; i < len(words); i += 2 {
		used[words[i]] = true
	}
	var choices []string
	for _, name := range networkFilters(words[2]) {
		if !used[name] {
			choices = append(choices, name)
		}
	}
	return choices
}
