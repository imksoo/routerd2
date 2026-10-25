package config

import (
	"encoding/hex"
	"errors"
)

func validateEventOutbox(c Committed) error {
	if len(c.EventOutbox) > MaxEventOutbox || c.Confirmation != nil && len(c.EventOutbox) == MaxEventOutbox {
		return errors.New("configuration event outbox exceeds capacity or rollback reserve")
	}
	seen := make(map[string]bool, len(c.EventOutbox))
	var previous uint64
	for _, event := range c.EventOutbox {
		id, err := hex.DecodeString(event.ID)
		if err != nil || len(id) != 16 || hex.EncodeToString(id) != event.ID || seen[event.ID] || event.Revision == 0 || event.Revision > c.Revision || event.Revision < previous || event.ObservedAt.IsZero() {
			return errors.New("invalid configuration event outbox identity or revision")
		}
		switch event.Type {
		case "committed", "confirmed", "rolled_back_timeout", "rolled_back_restart", "startup_selected":
		default:
			return errors.New("invalid configuration event type")
		}
		seen[event.ID] = true
		previous = event.Revision
	}
	return nil
}
