// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"encoding/binary"
	"errors"
)

const (
	wireAdminDown uint8 = iota
	wireDown
	wireInit
	wireUp
	flagPoll        = 0x20
	flagFinal       = 0x10
	flagIndependent = 0x08
	flagAuth        = 0x04
	flagDemand      = 0x02
	flagMultipoint  = 0x01
)

type control struct {
	state, diagnostic, flags, multiplier uint8
	my, your, minTx, minRx, minEchoRx    uint32
}

func parseControl(data []byte) (control, error) {
	var p control
	if len(data) < 24 || data[0]>>5 != 1 || data[3] != 24 || int(data[3]) > len(data) {
		return p, errors.New("invalid BFD version or unauthenticated control length")
	}
	p = control{state: data[1] >> 6, diagnostic: data[0] & 31, flags: data[1] & 63, multiplier: data[2], my: binary.BigEndian.Uint32(data[4:8]), your: binary.BigEndian.Uint32(data[8:12]), minTx: binary.BigEndian.Uint32(data[12:16]), minRx: binary.BigEndian.Uint32(data[16:20]), minEchoRx: binary.BigEndian.Uint32(data[20:24])}
	if p.flags&(flagAuth|flagDemand|flagMultipoint) != 0 {
		return control{}, errors.New("unsupported BFD authentication, demand or multipoint control")
	}
	if p.flags&(flagPoll|flagFinal) == flagPoll|flagFinal || p.multiplier == 0 || p.my == 0 || p.minTx == 0 || p.your == 0 && p.state != wireDown && p.state != wireAdminDown {
		return control{}, errors.New("invalid BFD control flags, discriminator or timing")
	}
	return p, nil
}

func (p control) bytes() []byte {
	out := make([]byte, 24)
	out[0], out[1], out[2], out[3] = 1<<5|p.diagnostic, p.state<<6|p.flags, p.multiplier, 24
	binary.BigEndian.PutUint32(out[4:8], p.my)
	binary.BigEndian.PutUint32(out[8:12], p.your)
	binary.BigEndian.PutUint32(out[12:16], p.minTx)
	binary.BigEndian.PutUint32(out[16:20], p.minRx)
	binary.BigEndian.PutUint32(out[20:24], p.minEchoRx)
	return out
}

func stateName(state uint8) string {
	return [...]string{StateAdminDown, StateDown, StateInit, StateUp}[state&3]
}
