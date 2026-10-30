//go:build linux

package health

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	old "github.com/imksoo/routerd/pkg/healthcheck"
)

// The request, TLS identity and status semantics follow routerd's
// ingressservice.Controller.checkHTTP. The existing Health worker owns the
// probe interval, thresholds, cancellation and finite observations.
func probeHTTP(ctx context.Context, c Config) old.ProbeResult {
	check := HTTPCheck{}
	if c.HTTP != nil {
		check = *c.HTTP
	}
	port := c.Port
	if port == 0 {
		port = 80
		if c.Protocol == "https" {
			port = 443
		}
	}
	path := check.Path
	if path == "" {
		path = "/"
	}
	target := net.JoinHostPort(c.Target, strconv.Itoa(port))
	u := url.URL{Scheme: c.Protocol, Host: target, Path: path}
	result := old.ProbeResult{ProbeEvidence: old.ProbeEvidence{EgressInterface: c.Interface, SourceAddress: c.SourceAddress}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err == nil {
		request.Host = strings.TrimSpace(check.Host)
		dialer := net.Dialer{Control: socketControl(c)}
		if c.SourceAddress != "" {
			dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(c.SourceAddress)}
		}
		transport := &http.Transport{DisableKeepAlives: true, DialContext: dialer.DialContext, TLSClientConfig: &tls.Config{ServerName: strings.TrimSpace(check.Host), InsecureSkipVerify: check.TLSSkipVerify}}
		defer transport.CloseIdleConnections()
		response, callErr := (&http.Client{Transport: transport}).Do(request)
		err = callErr
		if err == nil {
			response.Body.Close()
			accepted := response.StatusCode >= 200 && response.StatusCode < 400
			if len(check.ExpectedStatus) > 0 {
				accepted = slices.Contains(check.ExpectedStatus, response.StatusCode)
			}
			if !accepted {
				err = fmt.Errorf("http status %d", response.StatusCode)
			}
		}
	}
	result.OK = err == nil
	if err == nil {
		result.Message = "scoped HTTP request succeeded"
	} else {
		result.Message = err.Error()
		var timeout net.Error
		result.Timeout = errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
	}
	return result
}
