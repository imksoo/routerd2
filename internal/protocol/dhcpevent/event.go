// SPDX-License-Identifier: BSD-3-Clause

// Package dhcpevent defines the private dnsmasq notification contract.
package dhcpevent

import "time"

type Event struct {
	Action           string    `json:"action"`
	MAC              string    `json:"mac"`
	IP               string    `json:"ip"`
	Hostname         string    `json:"hostname,omitempty"`
	At               time.Time `json:"at"`
	FromFile         bool      `json:"from_file,omitempty"`
	VendorClass      string    `json:"vendor_class,omitempty"`
	RequestedOptions []int     `json:"requested_options,omitempty"`
}
type Reply struct {
	Error string `json:"error,omitempty"`
}
