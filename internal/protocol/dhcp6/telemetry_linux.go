//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package dhcp6

import (
	"context"
	"time"

	"github.com/imksoo/routerd2/internal/telemetry"
)

// A measurement ends only when its original transaction ends. A retransmission,
// ignored response or partial Reply does not complete that transaction. The
// protocol owner and its original lease deadlines remain unchanged.
func (c *client) recordExchange(ctx context.Context, before, after exchange, result string) {
	if before.sentAt.IsZero() || before.kind == after.kind && before.xid == after.xid {
		return
	}
	var operation string
	switch before.kind {
	case solicit:
		operation = "solicit"
	case request:
		operation = "request"
	case renew:
		operation = "renew"
	case rebind:
		operation = "rebind"
	case information:
		operation = "information_request"
	case confirm:
		operation = "confirm"
	case decline:
		operation = "decline"
	}
	// Use elapsed monotonic time anchored at the actual first send. UTC clock
	// corrections cannot make a completed exchange have a negative duration.
	telemetry.DHCPv6Exchange{Resource: c.cfg.Resource, Interface: c.cfg.Interface,
		Operation: operation, Result: result, StartedAt: before.sentAt,
		FinishedAt:    before.sentAt.Add(time.Since(before.sentAt)),
		Transmissions: before.attempts}.Record(ctx)
}
