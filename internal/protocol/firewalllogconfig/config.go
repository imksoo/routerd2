// SPDX-License-Identifier: BSD-3-Clause

// Package firewalllogconfig describes an NFLOG reader without linking it to boot.
package firewalllogconfig

import "errors"

type Config struct {
	Group uint16 `json:"group"`
}

func Validate(c Config) error {
	if c.Group == 0 {
		return errors.New("firewall log group must be 1..65535")
	}
	return nil
}
