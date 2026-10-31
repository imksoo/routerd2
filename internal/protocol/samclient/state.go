// SPDX-License-Identifier: BSD-3-Clause

package samclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
)

type savedState struct {
	ClaimDigest    string                   `json:"claim_digest"`
	IdentityDigest string                   `json:"identity_digest"`
	Revoked        bool                     `json:"revoked,omitempty"`
	Snapshot       samclientconfig.Snapshot `json:"snapshot"`
}

func (c *client) restore() error {
	data, err := os.ReadFile(c.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read SAM enrollment state: %w", err)
	}
	var saved savedState
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode SAM enrollment state: %w", err)
	}
	identity := samenrollment.ClientIdentityDigest(c.cfg.Claim)
	if saved.IdentityDigest != identity {
		return nil
	}
	if saved.Revoked {
		c.revoked = true
		c.snapshot.Phase = "revoked"
		return nil
	}
	if saved.ClaimDigest != c.digest {
		c.allowIdentityReplacement = true
		return nil
	}
	if saved.Snapshot.ClaimRef != c.snapshot.ClaimRef || saved.Snapshot.ClaimDigest != c.digest {
		return errors.New("SAM enrollment state has a different claim")
	}
	if err := samclientconfig.ValidateSnapshot(saved.Snapshot); err != nil {
		return fmt.Errorf("validate SAM enrollment state: %w", err)
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	if saved.Snapshot.RRSet != nil && saved.Snapshot.BootID == clock.BootID && saved.Snapshot.ExpiresNanos > uint64(clock.BoottimeNanoseconds) {
		c.snapshot = saved.Snapshot
	}
	return nil
}

func (c *client) save() error {
	c.mu.Lock()
	snapshot := c.snapshot
	revoked := c.revoked
	c.mu.Unlock()
	snapshot.LastError = ""
	// Direct peers need live agreement from every RR after a restart. The saved
	// RR fallback remains usable until its original deadline, but cannot revive
	// a previously agreed high-preference path while every RR is unreachable.
	snapshot.PeerGroup = nil
	if err := os.MkdirAll(filepath.Dir(c.cfg.StatePath), 0700); err != nil {
		return err
	}
	return config.AtomicJSON(c.cfg.StatePath, savedState{
		ClaimDigest:    c.digest,
		IdentityDigest: samenrollment.ClientIdentityDigest(c.cfg.Claim),
		Revoked:        revoked,
		Snapshot:       snapshot,
	})
}

func (c *client) withdraw() error {
	c.mu.Lock()
	c.revoked = true
	c.revokeDone = false
	c.snapshot.RRSet = nil
	c.snapshot.PeerGroup = nil
	c.snapshot.Phase = "revoked"
	c.snapshot.LastError = ""
	c.mu.Unlock()
	return c.save()
}
