// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

func showDHCPFingerprints(ctx context.Context, client *localapi.Client, args []string, out io.Writer) error {
	if len(args) == 0 || len(args)%2 != 1 {
		return errors.New("usage: show dhcp-fingerprints NAME [mac ADDRESS] [since RFC3339] [limit 1..1000]")
	}
	service := args[0]
	if !strings.HasPrefix(service, "service.") {
		service = "service." + service
	}
	query := url.Values{}
	for i := 1; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		if query.Has(key) {
			return errors.New("duplicate DHCP fingerprint filter")
		}
		switch key {
		case "mac", "since", "limit":
			query.Set(key, value)
		default:
			return errors.New("unknown DHCP fingerprint filter")
		}
	}
	// The history owner reads the existing DHCP observations and retains their
	// original timestamps, inference and truncation metadata.
	var history runtimeplan.DHCPFingerprintHistory
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(service)+"/dhcp-fingerprints?"+query.Encode(), nil, &history); err != nil {
		return err
	}
	return WriteJSON(out, history)
}
