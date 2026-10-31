// SPDX-License-Identifier: BSD-3-Clause
// Package eventgroupconfig contains configured federation identity and static peers.
package eventgroupconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/federation"
)

type Peer struct {
	NodeName        string   `json:"node_name"`
	Endpoint        string   `json:"endpoint"`
	Types           []string `json:"types,omitempty"`
	SubjectPrefixes []string `json:"subject_prefixes,omitempty"`
}
type Config struct {
	Group        string         `json:"group"`
	NodeName     string         `json:"node_name"`
	Listen       netip.AddrPort `json:"listen,omitempty"`
	SecretFile   string         `json:"secret_file"`
	ReplayWindow time.Duration  `json:"replay_window"`
	MaxEvents    int            `json:"max_events,omitempty"`
	MaxAge       time.Duration  `json:"max_age,omitempty"`
	Peers        []Peer         `json:"peers,omitempty"`
}
type Delivery struct {
	Key   string           `json:"key"`
	Event federation.Event `json:"event"`
}
type Page struct {
	More       bool       `json:"more"`
	Deliveries []Delivery `json:"deliveries"`
}
type Ack struct {
	Key string `json:"key"`
}

func Validate(c Config) error {
	if strings.TrimSpace(c.Group) == "" || strings.TrimSpace(c.NodeName) == "" {
		return errors.New("event group and node identity are required")
	}
	if c.Listen.IsValid() && c.Listen.Port() == 0 {
		return errors.New("event group listener requires a port")
	}
	if !filepath.IsAbs(c.SecretFile) {
		return errors.New("event group requires a provisioned HMAC secret file")
	}
	if c.ReplayWindow <= 0 || c.MaxEvents < 0 || c.MaxAge < 0 {
		return errors.New("invalid event group replay window or retention")
	}
	names := map[string]bool{}
	for _, p := range c.Peers {
		if p.NodeName == "" || p.NodeName == c.NodeName || names[p.NodeName] {
			return errors.New("event peers require distinct remote node identities")
		}
		names[p.NodeName] = true
		u, err := url.Parse(p.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("event peer requires an HTTP(S) endpoint")
		}
	}
	return nil
}
func Matches(p Peer, e federation.Event) bool {
	if len(p.Types) > 0 {
		found := false
		for _, v := range p.Types {
			if v == e.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(p.SubjectPrefixes) > 0 {
		for _, v := range p.SubjectPrefixes {
			if strings.HasPrefix(e.Subject, v) {
				return true
			}
		}
		return false
	}
	return true
}
func Consumer(service, peer string) string {
	sum := sha256.Sum256([]byte(service + "\x00" + peer))
	return "event-group:" + hex.EncodeToString(sum[:])
}
