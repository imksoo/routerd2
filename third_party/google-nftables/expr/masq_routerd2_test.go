// Copyright 2026 routerd2 contributors.
// Licensed under the Apache License, Version 2.0; see ../LICENSE.

package expr

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// These values describe the Linux nft_masq wire contract. In particular,
// registers alone do not enable translation into a protocol port range.
func TestRouterd2MasqueradeProtocolRangeFlags(t *testing.T) {
	for _, test := range []struct {
		name string
		masq Masq
		want map[uint16]uint32
	}{
		{"address-only", Masq{}, map[uint16]uint32{}},
		{"single-port", Masq{ToPorts: true, RegProtoMin: 2}, map[uint16]uint32{
			unix.NFTA_MASQ_FLAGS: unix.NF_NAT_RANGE_PROTO_SPECIFIED, unix.NFTA_MASQ_REG_PROTO_MIN: 2,
		}},
		{"port-range", Masq{ToPorts: true, RegProtoMin: 2, RegProtoMax: 3}, map[uint16]uint32{
			unix.NFTA_MASQ_FLAGS: unix.NF_NAT_RANGE_PROTO_SPECIFIED, unix.NFTA_MASQ_REG_PROTO_MIN: 2, unix.NFTA_MASQ_REG_PROTO_MAX: 3,
		}},
		{"flags-without-range", Masq{Random: true, FullyRandom: true, Persistent: true}, map[uint16]uint32{
			unix.NFTA_MASQ_FLAGS: unix.NF_NAT_RANGE_PROTO_RANDOM | unix.NF_NAT_RANGE_PROTO_RANDOM_FULLY | unix.NF_NAT_RANGE_PERSISTENT,
		}},
		{"flags-with-range", Masq{Random: true, FullyRandom: true, Persistent: true, ToPorts: true, RegProtoMin: 1, RegProtoMax: 2}, map[uint16]uint32{
			unix.NFTA_MASQ_FLAGS:         unix.NF_NAT_RANGE_PROTO_SPECIFIED | unix.NF_NAT_RANGE_PROTO_RANDOM | unix.NF_NAT_RANGE_PROTO_RANDOM_FULLY | unix.NF_NAT_RANGE_PERSISTENT,
			unix.NFTA_MASQ_REG_PROTO_MIN: 1, unix.NFTA_MASQ_REG_PROTO_MAX: 2,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire, err := MarshalExprData(unix.NFPROTO_INET, &test.masq)
			if err != nil {
				t.Fatal(err)
			}
			attrs, err := netlink.UnmarshalAttributes(wire)
			if err != nil {
				t.Fatal(err)
			}
			got := map[uint16]uint32{}
			for _, attr := range attrs {
				if len(attr.Data) != 4 {
					t.Fatalf("attribute %d has %d bytes", attr.Type, len(attr.Data))
				}
				if _, duplicate := got[attr.Type]; duplicate {
					t.Fatalf("duplicate attribute %d", attr.Type)
				}
				got[attr.Type] = binary.BigEndian.Uint32(attr.Data)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("masquerade attributes: got %v, want %v", got, test.want)
			}
			var roundTrip Masq
			if err := Unmarshal(unix.NFPROTO_INET, wire, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if roundTrip != test.masq {
				t.Fatalf("masquerade round trip: got %+v, want %+v", roundTrip, test.masq)
			}
		})
	}
}
