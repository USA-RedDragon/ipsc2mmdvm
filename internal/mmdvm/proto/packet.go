package proto

import (
	"fmt"
)

// DMRDSignature is the signature of an MMDVM DMR data packet.
const DMRDSignature = "DMRD"

type Packet struct {
	Signature   string
	Seq         uint
	Src         uint
	Dst         uint
	Repeater    uint
	Slot        bool
	GroupCall   bool
	FrameType   uint
	DTypeOrVSeq uint
	StreamID    uint
	DMRData     [33]byte
}

func (p Packet) Equal(other Packet) bool {
	if p.Signature != other.Signature {
		return false
	}
	if p.Seq != other.Seq {
		return false
	}
	if p.Src != other.Src {
		return false
	}
	if p.Dst != other.Dst {
		return false
	}
	if p.Repeater != other.Repeater {
		return false
	}
	if p.Slot != other.Slot {
		return false
	}
	if p.GroupCall != other.GroupCall {
		return false
	}
	if p.FrameType != other.FrameType {
		return false
	}
	if p.DTypeOrVSeq != other.DTypeOrVSeq {
		return false
	}
	if p.StreamID != other.StreamID {
		return false
	}
	if p.DMRData != other.DMRData {
		return false
	}
	return true
}

func Decode(data []byte) (Packet, bool) {
	var packet Packet
	if len(data) < 53 {
		return packet, false
	}
	if len(data) > 55 {
		return packet, false
	}
	packet.Signature = string(data[:4])
	packet.Seq = uint(data[4])
	packet.Src = uint(data[5])<<16 | uint(data[6])<<8 | uint(data[7])
	packet.Dst = uint(data[8])<<16 | uint(data[9])<<8 | uint(data[10])
	packet.Repeater = uint(data[11])<<24 | uint(data[12])<<16 | uint(data[13])<<8 | uint(data[14])
	bits := data[15]
	packet.Slot = (bits & 0x80) != 0
	packet.GroupCall = (bits & 0x40) == 0
	packet.FrameType = uint((bits & 0x30) >> 4)
	packet.DTypeOrVSeq = uint(bits & 0x0F)
	packet.StreamID = uint(data[16])<<24 | uint(data[17])<<16 | uint(data[18])<<8 | uint(data[19])
	copy(packet.DMRData[:], data[20:53])
	return packet, true
}

func (p *Packet) String() string {
	return fmt.Sprintf(
		"Packet: Seq %d, Src %d, Dst %d, Repeater %d, Slot %t, GroupCall %t, FrameType=%d, StreamId %d, DMRData %v",
		p.Seq, p.Src, p.Dst, p.Repeater, p.Slot, p.GroupCall, p.FrameType, p.StreamID, p.DMRData,
	)
}

func (p *Packet) Encode() []byte {
	// Encode the packet as we decoded
	data := make([]byte, 53)
	copy(data[:4], []byte(p.Signature))
	data[4] = byte(p.Seq & 0xFF)
	data[5] = byte((p.Src >> 16) & 0xFF)
	data[6] = byte((p.Src >> 8) & 0xFF)
	data[7] = byte(p.Src & 0xFF)
	data[8] = byte((p.Dst >> 16) & 0xFF)
	data[9] = byte((p.Dst >> 8) & 0xFF)
	data[10] = byte(p.Dst & 0xFF)
	data[11] = byte((p.Repeater >> 24) & 0xFF)
	data[12] = byte((p.Repeater >> 16) & 0xFF)
	data[13] = byte((p.Repeater >> 8) & 0xFF)
	data[14] = byte(p.Repeater & 0xFF)
	bits := byte(0)
	if p.Slot {
		bits |= 0x80
	}
	if !p.GroupCall {
		bits |= 0x40
	}
	bits |= byte((p.FrameType & 0x3) << 4)
	bits |= byte(p.DTypeOrVSeq & 0xF)
	data[15] = bits
	data[16] = byte((p.StreamID >> 24) & 0xFF)
	data[17] = byte((p.StreamID >> 16) & 0xFF)
	data[18] = byte((p.StreamID >> 8) & 0xFF)
	data[19] = byte(p.StreamID & 0xFF)
	copy(data[20:53], p.DMRData[:])
	return data
}
