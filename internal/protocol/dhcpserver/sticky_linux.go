// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/protocol/dhcpevent"
)

type leaseHistory struct {
	MAC       string    `json:"mac"`
	IP        string    `json:"ip"`
	Hostname  string    `json:"hostname,omitempty"`
	Until     time.Time `json:"until"`
	ContactAt time.Time `json:"contact_at"`
	RemovedAt time.Time `json:"removed_at"`
}

// Apply the event at the worker that owns this engine lifetime. The old
// routerd log store supplies the actual removal/hold and reacquisition rules.
func recordLeaseEvent(ctx context.Context, cfg Config, event dhcpevent.Event) error {
	// The pinned dnsmasq contract marks startup replay explicitly. Reading an
	// old file does not establish a new request and cannot cancel a removal.
	if event.Action == "old" && event.FromFile {
		return nil
	}
	if _, err := net.ParseMAC(event.MAC); err != nil {
		return err
	}
	if ip := net.ParseIP(event.IP); ip == nil || ip.To4() == nil || event.At.IsZero() {
		return errors.New("invalid DHCP lease event")
	}
	action := "added"
	switch event.Action {
	case "add", "old":
	case "del":
		action = "removed"
	default:
		return errors.New("unknown DHCP event")
	}
	if strings.ContainsAny(event.Hostname, "\x00\r\n") {
		event.Hostname = ""
	}
	log, err := logstore.OpenDHCPStickyLog(filepath.Join(filepath.Dir(cfg.StatePath), "dhcp-sticky.db"))
	if err != nil {
		return err
	}
	defer log.Close()
	return log.RecordLeaseEvent(ctx, action, event.MAC, event.IP, event.Hostname, cfg.StickyHoldDays, event.At)
}

func readHistory(ctx context.Context, cfg Config, now time.Time) ([]leaseHistory, error) {
	if cfg.StickyHoldDays == 0 {
		return nil, nil
	}
	path := filepath.Join(filepath.Dir(cfg.StatePath), "dhcp-sticky.db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	log, err := logstore.OpenDHCPStickyLogReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	rows, err := log.List(ctx, logstore.DHCPStickyFilter{HeldOnly: false, Now: now, Limit: 10000})
	if err != nil {
		return nil, err
	}
	holds := make([]leaseHistory, 0, len(rows))
	for _, row := range rows {
		holds = append(holds, leaseHistory{MAC: row.MAC, IP: row.IP, Hostname: row.Hostname, Until: row.StickyUntil, ContactAt: row.LastRequestAt, RemovedAt: row.ReleasedAt})
	}
	slices.SortFunc(holds, func(a, b leaseHistory) int { return strings.Compare(a.MAC+"|"+a.IP, b.MAC+"|"+b.IP) })
	return holds, nil
}

func saveHistory(ctx context.Context, cfg Config, history []leaseHistory) error {
	if cfg.StickyHoldDays == 0 {
		return nil
	}
	log, err := logstore.OpenDHCPStickyLog(filepath.Join(filepath.Dir(cfg.StatePath), "dhcp-sticky.db"))
	if err != nil {
		return err
	}
	if err := log.Close(); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(cfg.StatePath), "dhcp-sticky.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.UnixNano()
	}
	for _, h := range history {
		_, err := tx.ExecContext(ctx, `INSERT INTO dhcp_sticky_leases(mac,ip,hostname,family,last_request_at,released_at,sticky_until)
VALUES(?,?,?,'ipv4',?,?,?) ON CONFLICT(mac,ip) DO UPDATE SET hostname=excluded.hostname,last_request_at=excluded.last_request_at,released_at=excluded.released_at,sticky_until=excluded.sticky_until`, h.MAC, h.IP, h.Hostname, stamp(h.ContactAt), stamp(h.RemovedAt), stamp(h.Until))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Only a later, actually observed request clears a removal. An unexpired row
// in a peer's older lease file supplies no evidence of reacquisition.
func mergeHistory(a, b []leaseHistory) []leaseHistory {
	byBinding := map[string]leaseHistory{}
	for _, history := range [][]leaseHistory{a, b} {
		for _, h := range history {
			key := h.MAC + "|" + h.IP
			old, ok := byBinding[key]
			if !ok {
				old = h
			} else {
				if h.ContactAt.After(old.ContactAt) {
					old.ContactAt = h.ContactAt
					if h.Hostname != "" {
						old.Hostname = h.Hostname
					}
				}
				if h.RemovedAt.After(old.RemovedAt) {
					old.RemovedAt = h.RemovedAt
					old.Until = h.Until
					if h.Hostname != "" {
						old.Hostname = h.Hostname
					}
				}
			}
			if !old.RemovedAt.After(old.ContactAt) {
				old.Until = time.Time{}
			}
			byBinding[key] = old
		}
	}
	result := make([]leaseHistory, 0, len(byBinding))
	for _, h := range byBinding {
		result = append(result, h)
	}
	slices.SortFunc(result, func(a, b leaseHistory) int { return strings.Compare(a.MAC+"|"+a.IP, b.MAC+"|"+b.IP) })
	return result
}

// A daemon may have stopped before the engine emitted its expiry event. Use
// the stored grant's expiry for that removal, so downtime never renews a hold.
func expiredHistory(cfg Config, leases string, now time.Time) []leaseHistory {
	if cfg.StickyHoldDays == 0 {
		return nil
	}
	var holds []leaseHistory
	for _, line := range strings.Split(leases, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			continue
		}
		expires, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || expires <= 0 || expires > now.Unix() {
			continue
		}
		until := time.Unix(expires, 0).UTC().Add(time.Duration(cfg.StickyHoldDays) * 24 * time.Hour)
		if !until.After(now) {
			continue
		}
		hostname := fields[3]
		if hostname == "*" {
			hostname = ""
		}
		holds = append(holds, leaseHistory{MAC: fields[1], IP: fields[2], Hostname: hostname, Until: until, RemovedAt: time.Unix(expires, 0).UTC()})
	}
	return holds
}

// Adapted from routerd's writeDnsmasqHostsFile/dnsmasqStickyHostLines. Held
// addresses are dnsmasq host reservations; explicit operator reservations win.
func updateHeldHosts(ctx context.Context, cfg Config) (bool, error) {
	if cfg.StickyHoldDays == 0 {
		return false, nil
	}
	holds, err := readHistory(ctx, cfg, time.Now())
	if err != nil {
		return false, err
	}
	var lines []string
	for _, hold := range holds {
		if !hold.Until.After(time.Now()) {
			continue
		}
		reserved := false
		for _, reservation := range cfg.Reservations {
			if strings.EqualFold(reservation.MACAddress, hold.MAC) || reservation.IPAddress == hold.IP {
				reserved = true
				break
			}
		}
		if reserved {
			continue
		}
		parts := []string{hold.MAC, hold.IP}
		if hold.Hostname != "" {
			parts = append(parts, hold.Hostname)
		}
		parts = append(parts, "12h")
		lines = append(lines, strings.Join(parts, ","))
	}
	slices.Sort(lines)
	data := []byte(strings.Join(lines, "\n") + "\n")
	path := filepath.Join(filepath.Dir(cfg.StatePath), "held.hosts")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if bytes.Equal(old, data) {
		return false, nil
	}
	return true, replaceFile(path, data)
}

func normalizeHistory(holds []leaseHistory) error {
	for i := range holds {
		h := &holds[i]
		mac, err := net.ParseMAC(h.MAC)
		if err != nil {
			return err
		}
		ip := net.ParseIP(h.IP)
		if ip == nil || ip.To4() == nil || h.ContactAt.IsZero() && h.RemovedAt.IsZero() {
			return errors.New("invalid DHCP sticky binding")
		}
		h.MAC = mac.String()
		h.IP = ip.To4().String()
		h.Until = h.Until.UTC()
		h.ContactAt = h.ContactAt.UTC()
		h.RemovedAt = h.RemovedAt.UTC()
		if strings.ContainsAny(h.Hostname, "\x00\r\n") {
			h.Hostname = ""
		}
	}
	return nil
}
