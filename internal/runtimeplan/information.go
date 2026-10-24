package runtimeplan

import (
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
)

// DHCPv6 information refresh is not itself a lease expiry. The controller's
// explicit freshness policy stops using learned information at that deadline;
// only the protocol's explicit infinity value permits a zero deadline. An
// active PD lease still bounds information learned on its shared socket.
func informationDeadline(snapshot dhcp6pd.Snapshot, now time.Time) (time.Time, bool) {
	if !snapshot.Active || snapshot.InfoUpdatedAt.IsZero() || snapshot.InformationStale {
		return time.Time{}, false
	}
	until := snapshot.InformationRefreshAt
	if until.IsZero() && snapshot.InformationRefreshSeconds != ^uint32(0) {
		return time.Time{}, false
	}
	if !snapshot.InformationOnly {
		if snapshot.ExpiresAt.IsZero() {
			return time.Time{}, false
		}
		until = deadlineMinimum(until, snapshot.ExpiresAt)
	}
	return until, deadlineLive(until, now)
}
