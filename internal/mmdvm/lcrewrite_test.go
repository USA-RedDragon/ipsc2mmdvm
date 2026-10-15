package mmdvm

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"sync"
	"testing"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// ipscCall builds the IPSC packets of one voice call: a voice header, bursts
// A-F, and a terminator. lcHeader and lcTerm are the 12-byte full LCs.
func ipscCall(src, dst uint32, group bool, lcHeader, lcTerm []byte) [][]byte {
	pt := byte(0x81)
	if group {
		pt = 0x80
	}
	base := func(size int, burstType byte, rtpSeq uint16) []byte {
		b := make([]byte, size)
		b[0] = pt
		b[6], b[7], b[8] = byte(src>>16&0xFF), byte(src>>8&0xFF), byte(src&0xFF)
		b[9], b[10], b[11] = byte(dst>>16&0xFF), byte(dst>>8&0xFF), byte(dst&0xFF)
		b[12] = 0x01
		if group {
			b[12] = 0x02
		}
		binary.BigEndian.PutUint32(b[13:17], 0x1234)
		b[18] = 0x80
		binary.BigEndian.PutUint16(b[20:22], rtpSeq)
		b[30] = burstType
		return b
	}
	hdr := base(54, 0x01, 0)
	copy(hdr[38:50], lcHeader)
	out := [][]byte{hdr}
	sizes := [6]int{52, 57, 57, 57, 66, 57}
	lcss := [6]byte{0, 1, 3, 3, 2, 0}
	for pos := range sizes {
		b := base(sizes[pos], 0x0A, uint16(pos+1)) //nolint:gosec // pos is in [0,5]
		if pos != 0 {
			b[len(b)-1] = 0x10 | lcss[pos]<<1
		}
		out = append(out, b)
	}
	term := base(54, 0x02, 7)
	term[17] = 0x40
	copy(term[38:50], lcTerm)
	return append(out, term)
}

// embeddedFragment returns the 32 embedded signalling bits of a DMR voice
// burst.
func embeddedFragment(data [33]byte) []byte {
	var out [4]byte
	for i := 0; i < 32; i++ {
		bit := 116 + i
		if data[bit/8]>>(7-bit%8)&1 == 1 {
			out[i/8] |= 1 << (7 - i%8)
		}
	}
	return out[:]
}

// TestSrcRewriteRewritesLinkControlToIPSC checks that a private call that a
// SrcRewrite turns into a group call reaches IPSC with a group-call full LC
// to the new talkgroup, the original service options, and fresh parity, in
// the voice header, the terminator, and the embedded LC of bursts B-E.
func TestSrcRewriteRewritesLinkControlToIPSC(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	c.state.Store(uint32(STATE_READY))
	c.netRewrites = []rewrite.Rule{&rewrite.SrcRewrite{Name: testRuleName, FromSlot: 1, FromID: 1234, ToSlot: 1, ToTG: 9, Range: 1}}
	var mu sync.Mutex
	var got [][]byte
	c.SetIPSCHandler(func(_ context.Context, d []byte) {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
	})

	// Private call 1234 -> 5555, emergency service option, valid parity.
	call := ipscCall(1234, 5555, false,
		mustHex(t, "0300800015B30004D265893D"), mustHex(t, "0300800015B30004D26A8632"))
	src, err := ipsc.NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range call {
		for _, pkt := range src.TranslateToMMDVM(p[0], p) {
			pkt.StreamID = 0x77
			c.handleReady(pkt.Encode())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3+6+1 {
		t.Fatalf("expected 10 IPSC packets, got %d", len(got))
	}
	for i := 0; i < 3; i++ {
		if got[i][0] != 0x80 || hex.EncodeToString(got[i][38:50]) != "0000800000090004d26b6a08" {
			t.Fatalf("header %d: type 0x%02X LC % X, want group call LC 00 00 80 00 00 09 00 04 D2 6B 6A 08", i, got[i][0], got[i][38:50])
		}
	}
	burstE := got[3+4]
	if len(burstE) != 66 || hex.EncodeToString(burstE[56:59]) != "000080" {
		t.Fatalf("burst E LC prefix % X, want 00 00 80", burstE[56:59])
	}
	// Embedded LC fragments of 00 00 80 00 00 09 00 04 D2.
	for i, want := range []string{"00000a03", "004b0000", "03051441", "0f531e53"} {
		if g := hex.EncodeToString(got[3+1+i][52:56]); g != want {
			t.Fatalf("burst %c embedded fragment %s, want %s", 'B'+i, g, want)
		}
	}
	term := got[len(got)-1]
	if hex.EncodeToString(term[38:50]) != "0000800000090004d2646507" {
		t.Fatalf("terminator LC % X, want 00 00 80 00 00 09 00 04 D2 64 65 07", term[38:50])
	}
}

// TestTGRewriteRewritesLinkControlToMMDVM checks that an IPSC group call that
// a TGRewrite moves to another talkgroup reaches the MMDVM master with that
// talkgroup in the full LC and the embedded LC, keeping the feature set ID
// and service options.
func TestTGRewriteRewritesLinkControlToMMDVM(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	c.started.Store(true)
	c.rfRewrites = []rewrite.Rule{&rewrite.TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 3120, ToSlot: 1, ToTG: 91, Range: 1}}
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}

	// Captured MOTOTRBO group call LC: Motorola FID, TG 3120.
	call := ipscCall(0x2F9BE5, 3120, true,
		mustHex(t, "001020000C302F9BE5DAD45A"), mustHex(t, "001020000C302F9BE5D5DB55"))
	var out []proto.Packet
	for _, p := range call {
		c.HandleIPSCBurst(p[0], p, addr)
		for len(c.tx_chan) > 0 {
			out = append(out, <-c.tx_chan)
		}
	}
	if len(out) != 1+6+1 {
		t.Fatalf("expected 8 MMDVM packets, got %d", len(out))
	}

	// A fresh translator passes a full LC through only when its parity is
	// valid, so the IPSC bytes show the LC the master received.
	check, err := ipsc.NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	hdr := check.TranslateToIPSC(out[0])
	if len(hdr) != 3 || hex.EncodeToString(hdr[0][38:50]) != "00102000005b2f9be585317d" {
		t.Fatalf("header LC % X, want 00 10 20 00 00 5B 2F 9B E5 85 31 7D", hdr[0][38:50])
	}
	// Embedded LC fragments of 00 10 20 00 00 5B 2F 9B E5.
	for i, want := range []string{"470f0f06", "0f090047", "0c121803", "17421747"} {
		if g := hex.EncodeToString(embeddedFragment(out[2+i].DMRData)); g != want {
			t.Fatalf("burst %c embedded fragment %s, want %s", 'B'+i, g, want)
		}
	}
	term := check.TranslateToIPSC(out[len(out)-1])
	if len(term) != 1 || hex.EncodeToString(term[0][38:50]) != "00102000005b2f9be58a3e72" {
		t.Fatalf("terminator LC % X, want 00 10 20 00 00 5B 2F 9B E5 8A 3E 72", term[0][38:50])
	}
}
