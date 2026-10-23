// SPDX-License-Identifier: BSD-3-Clause

package dhcpserverconfig

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

var optionNames = map[string]byte{
	"subnet-mask": 1, "time-offset": 2, "router": 3, "time-server": 4, "name-server": 5,
	"dns-server": 6, "log-server": 7, "cookie-server": 8, "lpr-server": 9,
	"impress-server": 10, "resource-location-server": 11, "hostname": 12,
	"boot-file-size": 13, "merit-dump": 14, "domain-name": 15, "swap-server": 16,
	"root-path": 17, "extension-path": 18, "ip-forward-enable": 19,
	"non-local-source-routing": 20, "policy-filter": 21,
	"max-datagram-reassembly": 22, "default-ttl": 23, "path-mtu-aging-timeout": 24,
	"path-mtu-plateau-table": 25, "mtu": 26, "all-subnets-local": 27, "broadcast": 28,
	"mask-discovery": 29, "mask-supplier": 30, "router-discovery": 31,
	"router-solicitation": 32, "static-route": 33, "trailer-encapsulation": 34,
	"arp-cache-timeout": 35, "ethernet-encap": 36, "tcp-ttl": 37,
	"tcp-keepalive": 38, "tcp-keepalive-garbage": 39, "nis-domain": 40,
	"nis-server": 41, "ntp-server": 42, "vendor-encap": 43, "netbios-ns": 44,
	"netbios-dd": 45, "netbios-node-type": 46, "netbios-scope": 47,
	"x-windows-fs": 48, "x-windows-dm": 49, "vendor-class": 60,
	"nisplus-domain": 64, "nisplus-server": 65, "tftp-server": 66, "bootfile-name": 67,
	"mobile-ip-home": 68, "smtp-server": 69, "pop3-server": 70, "nntp-server": 71,
	"www-server": 72, "finger-server": 73, "irc-server": 74, "streettalk-server": 75,
	"streettalk-directory-assistance": 76, "user-class": 77,
	"domain-search": 119, "classless-static-route": 121,
	"ms-classless-static-route": 249,
}

// EncodeOptions converts the original api.DHCPv4OptionSpec into wire values.
// It supports typed familiar dnsmasq names, and explicit hex:<bytes> for opaque
// numbered options. Backend-specific text is never guessed or executed.
func EncodeOptions(options []Option) (map[byte][]byte, error) {
	if len(options) > 128 {
		return nil, errors.New("DHCP options are limited to 128 entries")
	}
	out := make(map[byte][]byte, len(options))
	total := 0
	for _, o := range options {
		code := o.Code
		if o.Name != "" {
			n, ok := optionNames[strings.ToLower(strings.TrimSpace(o.Name))]
			if !ok {
				return nil, fmt.Errorf("unsupported DHCP option name %q; use a numbered hex value for an opaque option", o.Name)
			}
			if code != 0 && code != int(n) {
				return nil, errors.New("DHCP option name and code disagree")
			}
			code = int(n)
		}
		if code < 1 || code > 254 {
			return nil, errors.New("DHCP option code must be 1..254")
		}
		switch code {
		case 1, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 61, 80, 82, 90:
			return nil, fmt.Errorf("DHCP option %d is protocol-owned or unsupported; use the typed server configuration", code)
		}
		if _, ok := out[byte(code)]; ok {
			return nil, fmt.Errorf("duplicate DHCP option %d", code)
		}
		value, err := encodeOption(byte(code), o.Value)
		if err != nil {
			return nil, fmt.Errorf("DHCP option %d: %w", code, err)
		}
		if len(value) > 1024 {
			return nil, errors.New("DHCP option value exceeds 1024 bytes")
		}
		total += len(value) + 2
		if total > 4096 {
			return nil, errors.New("DHCP configured option values exceed 4096 bytes")
		}
		out[byte(code)] = value
	}
	return out, nil
}

func encodeOption(code byte, value string) ([]byte, error) {
	v := strings.TrimSpace(value)
	if strings.HasPrefix(v, "hex:") {
		s := strings.NewReplacer(":", "", " ", "").Replace(strings.TrimPrefix(v, "hex:"))
		b, e := hex.DecodeString(s)
		if e != nil {
			return nil, errors.New("invalid hexadecimal option value")
		}
		if e := validateOptionLength(code, b); e != nil {
			return nil, e
		}
		return b, nil
	}
	switch code {
	case 3, 4, 5, 6, 7, 8, 9, 10, 11, 16, 28, 32, 41, 42, 44, 45, 48, 49, 65, 68, 69, 70, 71, 72, 73, 74, 75, 76:
		if v == "" {
			return []byte{}, nil
		} // Explicitly suppress a default option.
		var b []byte
		for _, p := range strings.Split(v, ",") {
			a, e := ipv4(strings.TrimSpace(p))
			if e != nil {
				return nil, errors.New("expected comma-separated unicast IPv4 addresses")
			}
			n := a.As4()
			b = append(b, n[:]...)
		}
		if (code == 16 || code == 28 || code == 32) && len(b) != 4 {
			return nil, errors.New("expected exactly one IPv4 address")
		}
		return b, nil
	case 12, 14, 15, 17, 18, 40, 47, 60, 64, 66, 67, 77:
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("text option contains control characters")
		}
		if (code == 12 || code == 15) && value != "" && !ValidHostname(value) {
			return nil, errors.New("invalid domain or hostname")
		}
		return []byte(value), nil
	case 2:
		n, e := strconv.ParseInt(v, 10, 32)
		if e != nil {
			return nil, errors.New("expected signed 32-bit integer")
		}
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(n))
		return b, nil
	case 13, 22, 26:
		return unsignedOption(v, 2)
	case 19, 20, 27, 29, 30, 31, 34, 36, 39:
		switch v {
		case "1", "true", "yes":
			return []byte{1}, nil
		case "0", "false", "no":
			return []byte{0}, nil
		}
		return nil, errors.New("expected boolean 0 or 1")
	case 23, 37, 46:
		return unsignedOption(v, 1)
	case 24, 35, 38:
		return unsignedOption(v, 4)
	case 25:
		var b []byte
		for _, p := range strings.Split(v, ",") {
			n, e := unsignedOption(strings.TrimSpace(p), 2)
			if e != nil {
				return nil, e
			}
			b = append(b, n...)
		}
		return b, nil
	case 21, 33:
		var b []byte
		parts := strings.Split(v, ",")
		if len(parts) == 0 || len(parts)%2 != 0 {
			return nil, errors.New("expected pairs of IPv4 addresses")
		}
		for _, p := range parts {
			a, e := netip.ParseAddr(strings.TrimSpace(p))
			if e != nil || !a.Is4() {
				return nil, errors.New("invalid IPv4 pair")
			}
			n := a.As4()
			b = append(b, n[:]...)
		}
		return b, nil
	case 119:
		var b []byte
		for _, domain := range strings.Split(v, ",") {
			domain = strings.TrimSuffix(strings.TrimSpace(domain), ".")
			if !ValidHostname(domain) {
				return nil, errors.New("invalid domain-search name")
			}
			for _, label := range strings.Split(domain, ".") {
				b = append(b, byte(len(label)))
				b = append(b, []byte(label)...)
			}
			b = append(b, 0)
		}
		return b, nil
	case 121, 249:
		parts := strings.Split(v, ",")
		if len(parts) == 0 || len(parts)%2 != 0 {
			return nil, errors.New("expected prefix,gateway pairs")
		}
		var b []byte
		for i := 0; i < len(parts); i += 2 {
			p, e := netip.ParsePrefix(strings.TrimSpace(parts[i]))
			if e != nil || !p.Addr().Is4() || p != p.Masked() {
				return nil, errors.New("classless route requires a canonical IPv4 prefix")
			}
			g, e := netip.ParseAddr(strings.TrimSpace(parts[i+1]))
			if e != nil || !g.Is4() || g.IsMulticast() {
				return nil, errors.New("classless route gateway must be IPv4")
			}
			b = append(b, byte(p.Bits()))
			a := p.Addr().As4()
			b = append(b, a[:(p.Bits()+7)/8]...)
			a = g.As4()
			b = append(b, a[:]...)
		}
		return b, nil
	default:
		return nil, errors.New("opaque numbered options require an explicit hex: value")
	}
}
func unsignedOption(v string, n int) ([]byte, error) {
	x, e := strconv.ParseUint(v, 10, n*8)
	if e != nil {
		return nil, errors.New("integer option is outside its wire width")
	}
	b := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		b[i] = byte(x)
		x >>= 8
	}
	return b, nil
}
func validateOptionLength(code byte, b []byte) error {
	switch code {
	case 3, 4, 5, 6, 7, 8, 9, 10, 11, 41, 42, 44, 45, 48, 49, 65, 68, 69, 70, 71, 72, 73, 74, 75, 76:
		if len(b)%4 != 0 {
			return errors.New("IPv4 list option has an invalid length")
		}
	case 16, 28, 32, 2, 24, 35, 38:
		if len(b) != 4 {
			return errors.New("option requires four bytes")
		}
	case 13, 22, 26:
		if len(b) != 2 {
			return errors.New("option requires two bytes")
		}
	case 19, 20, 23, 27, 29, 30, 31, 34, 36, 37, 39, 46:
		if len(b) != 1 {
			return errors.New("option requires one byte")
		}
	case 25:
		if len(b)%2 != 0 {
			return errors.New("16-bit list option has invalid length")
		}
	case 21, 33:
		if len(b)%8 != 0 {
			return errors.New("IPv4 pair option has invalid length")
		}
	}
	return nil
}
