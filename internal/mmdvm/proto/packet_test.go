package proto

import (
	"strings"
	"testing"
)

func samplePacket() Packet {
	var dmr [33]byte
	for i := range dmr {
		dmr[i] = byte(i)
	}
	return Packet{
		Signature:   DMRDSignature,
		Seq:         42,
		Src:         123456,
		Dst:         654321,
		Repeater:    3001,
		Slot:        true,
		GroupCall:   true,
		FrameType:   1,
		DTypeOrVSeq: 3,
		StreamID:    0xDEADBEEF,
		DMRData:     dmr,
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	original := samplePacket()
	encoded := original.Encode()

	if len(encoded) != 53 {
		t.Fatalf("expected 53 bytes, got %d", len(encoded))
	}

	decoded, ok := Decode(encoded)
	if !ok {
		t.Fatal("Decode returned false")
	}

	if !original.Equal(decoded) {
		t.Fatalf("round-trip failed:\n  original: %+v\n  decoded:  %+v", original, decoded)
	}
}

func TestDecodeSignature(t *testing.T) {
	t.Parallel()
	p := samplePacket()
	data := p.Encode()
	decoded, ok := Decode(data)
	if !ok {
		t.Fatal("Decode returned false")
	}
	if decoded.Signature != DMRDSignature {
		t.Fatalf("expected signature DMRD, got %q", decoded.Signature)
	}
}

func TestDecodeSeq(t *testing.T) {
	t.Parallel()
	p := samplePacket()
	p.Seq = 0
	decoded, ok := Decode(p.Encode())
	if !ok {
		t.Fatal("Decode returned false")
	}
	if decoded.Seq != 0 {
		t.Fatalf("expected seq 0, got %d", decoded.Seq)
	}

	p.Seq = 255
	decoded, ok = Decode(p.Encode())
	if !ok {
		t.Fatal("Decode returned false")
	}
	if decoded.Seq != 255 {
		t.Fatalf("expected seq 255, got %d", decoded.Seq)
	}
}

func TestDecodeSrcDst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  uint
		dst  uint
	}{
		{"zeros", 0, 0},
		{"max 24-bit", 0xFFFFFF, 0xFFFFFF},
		{"typical", 3141234, 91},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := samplePacket()
			p.Src = tt.src
			p.Dst = tt.dst
			decoded, ok := Decode(p.Encode())
			if !ok {
				t.Fatal("Decode returned false")
			}
			if decoded.Src != tt.src {
				t.Fatalf("expected src %d, got %d", tt.src, decoded.Src)
			}
			if decoded.Dst != tt.dst {
				t.Fatalf("expected dst %d, got %d", tt.dst, decoded.Dst)
			}
		})
	}
}

func TestDecodeRepeater(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		repeater uint
	}{
		{"zero", 0},
		{"max", 0xFFFFFFFF},
		{"typical", 311860},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := samplePacket()
			p.Repeater = tt.repeater
			decoded, ok := Decode(p.Encode())
			if !ok {
				t.Fatal("Decode returned false")
			}
			if decoded.Repeater != tt.repeater {
				t.Fatalf("expected repeater %d, got %d", tt.repeater, decoded.Repeater)
			}
		})
	}
}

func TestDecodeSlot(t *testing.T) {
	t.Parallel()
	for _, slot := range []bool{true, false} {
		p := samplePacket()
		p.Slot = slot
		decoded, ok := Decode(p.Encode())
		if !ok {
			t.Fatal("Decode returned false")
		}
		if decoded.Slot != slot {
			t.Fatalf("expected slot %v, got %v", slot, decoded.Slot)
		}
	}
}

func TestDecodeGroupCall(t *testing.T) {
	t.Parallel()
	for _, gc := range []bool{true, false} {
		p := samplePacket()
		p.GroupCall = gc
		decoded, ok := Decode(p.Encode())
		if !ok {
			t.Fatal("Decode returned false")
		}
		if decoded.GroupCall != gc {
			t.Fatalf("expected group call %v, got %v", gc, decoded.GroupCall)
		}
	}
}

func TestDecodeFrameType(t *testing.T) {
	t.Parallel()
	for ft := uint(0); ft <= 3; ft++ {
		p := samplePacket()
		p.FrameType = ft
		decoded, ok := Decode(p.Encode())
		if !ok {
			t.Fatal("Decode returned false")
		}
		if decoded.FrameType != ft {
			t.Fatalf("expected frame type %d, got %d", ft, decoded.FrameType)
		}
	}
}

func TestDecodeDTypeOrVSeq(t *testing.T) {
	t.Parallel()
	for d := uint(0); d <= 15; d++ {
		p := samplePacket()
		p.DTypeOrVSeq = d
		decoded, ok := Decode(p.Encode())
		if !ok {
			t.Fatal("Decode returned false")
		}
		if decoded.DTypeOrVSeq != d {
			t.Fatalf("expected DTypeOrVSeq %d, got %d", d, decoded.DTypeOrVSeq)
		}
	}
}

func TestDecodeStreamID(t *testing.T) {
	t.Parallel()
	tests := []uint{0, 1, 0xFFFFFFFF, 0xDEADBEEF}
	for _, sid := range tests {
		p := samplePacket()
		p.StreamID = sid
		decoded, ok := Decode(p.Encode())
		if !ok {
			t.Fatal("Decode returned false")
		}
		if decoded.StreamID != sid {
			t.Fatalf("expected stream ID %d, got %d", sid, decoded.StreamID)
		}
	}
}

func TestDecodeDMRData(t *testing.T) {
	t.Parallel()
	p := samplePacket()
	for i := range p.DMRData {
		p.DMRData[i] = byte(0xAA)
	}
	decoded, ok := Decode(p.Encode())
	if !ok {
		t.Fatal("Decode returned false")
	}
	if decoded.DMRData != p.DMRData {
		t.Fatalf("DMRData mismatch")
	}
}

func TestDecodeTooShort(t *testing.T) {
	t.Parallel()
	data := make([]byte, 52)
	_, ok := Decode(data)
	if ok {
		t.Fatal("expected Decode to fail on short packet")
	}
}

func TestDecodeTooLong(t *testing.T) {
	t.Parallel()
	data := make([]byte, 56)
	_, ok := Decode(data)
	if ok {
		t.Fatal("expected Decode to fail on long packet")
	}
}

func TestDecodeExact53(t *testing.T) {
	t.Parallel()
	data := make([]byte, 53)
	copy(data[:4], DMRDSignature)
	_, ok := Decode(data)
	if !ok {
		t.Fatal("expected Decode to succeed on 53-byte packet")
	}
}

func TestDecodeAccepts54And55(t *testing.T) {
	t.Parallel()
	for size := 54; size <= 55; size++ {
		data := make([]byte, size)
		copy(data[:4], DMRDSignature)
		_, ok := Decode(data)
		if !ok {
			t.Fatalf("expected Decode to succeed on %d-byte packet", size)
		}
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()
	a := samplePacket()
	b := samplePacket()
	if !a.Equal(b) {
		t.Fatal("identical packets should be equal")
	}
}

func TestNotEqual(t *testing.T) {
	t.Parallel()
	modifications := []struct {
		name   string
		modify func(*Packet)
	}{
		{"signature", func(p *Packet) { p.Signature = "XXXX" }},
		{"seq", func(p *Packet) { p.Seq = 99 }},
		{"src", func(p *Packet) { p.Src = 999 }},
		{"dst", func(p *Packet) { p.Dst = 999 }},
		{"repeater", func(p *Packet) { p.Repeater = 999 }},
		{"slot", func(p *Packet) { p.Slot = !p.Slot }},
		{"groupCall", func(p *Packet) { p.GroupCall = !p.GroupCall }},
		{"frameType", func(p *Packet) { p.FrameType = 99 }},
		{"dtypeOrVSeq", func(p *Packet) { p.DTypeOrVSeq = 99 }},
		{"streamID", func(p *Packet) { p.StreamID = 999 }},
		{"dmrData", func(p *Packet) { p.DMRData[0] = 0xFF }},
	}
	for _, tt := range modifications {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := samplePacket()
			b := samplePacket()
			tt.modify(&b)
			if a.Equal(b) {
				t.Fatalf("packets should differ after modifying %s", tt.name)
			}
		})
	}
}

func TestString(t *testing.T) {
	t.Parallel()
	p := samplePacket()
	s := p.String()
	if s == "" {
		t.Fatal("String() returned empty string")
	}
	expected := []string{"Seq", "Src", "Dst", "Repeater", "Slot", "GroupCall", "FrameType", "StreamId"}
	for _, e := range expected {
		if !strings.Contains(s, e) {
			t.Fatalf("String() output missing %q: %s", e, s)
		}
	}
}

func TestEncodeBitFields(t *testing.T) {
	t.Parallel()
	p := Packet{
		Signature:   DMRDSignature,
		Slot:        true,
		GroupCall:   false,
		FrameType:   2,
		DTypeOrVSeq: 5,
	}
	data := p.Encode()
	bits := data[15]

	// Bit 7: Slot (0x80)
	if bits&0x80 != 0x80 {
		t.Fatalf("expected bit 7 set for Slot=true, got %08b", bits)
	}
	// Bit 6: Call type (0x40 = private)
	if bits&0x40 != 0x40 {
		t.Fatalf("expected bit 6 set for GroupCall=false, got %08b", bits)
	}
	// Bits 5-4: FrameType
	if (bits&0x30)>>4 != 2 {
		t.Fatalf("expected FrameType=2, got %d from bits %08b", (bits&0x30)>>4, bits)
	}
	// Bits 3-0: DTypeOrVSeq
	if bits&0x0F != 5 {
		t.Fatalf("expected DTypeOrVSeq=5, got %d from bits %08b", bits&0x0F, bits)
	}
}

func TestEncodeLen(t *testing.T) {
	t.Parallel()
	p := samplePacket()
	data := p.Encode()
	if len(data) != 53 {
		t.Fatalf("expected encoded length 53, got %d", len(data))
	}
}
