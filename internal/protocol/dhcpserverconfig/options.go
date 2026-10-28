// SPDX-License-Identifier: BSD-3-Clause

package dhcpserverconfig

import (
	"errors"
	"strings"
)

// ValidateOptions protects the config-file boundary. dnsmasq interprets option
// syntax, including its existing named, numeric, quoted and byte forms.
func ValidateOptions(options []Option) error {
	for _, option := range options {
		if option.Name == "" && (option.Code < 1 || option.Code > 254) {
			return errors.New("DHCP option requires a name or code 1..254")
		}
		if strings.ContainsAny(option.Name, ",\x00\r\n") || strings.ContainsAny(option.Value, "\x00\r\n") {
			return errors.New("DHCP option contains a config delimiter")
		}
	}
	return nil
}
