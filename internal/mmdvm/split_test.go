package mmdvm

import (
	"bytes"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/ipsc"
)

func TestXPRSplitCallToMaster(t *testing.T) {
	t.Parallel()
	_, golden := loadSidebandGolden(t)
	alias := golden[0]
	for _, g := range loadXPRSplitGroups(t) {
		for _, ts := range []bool{false, true} {
			t.Run(g.name, func(t *testing.T) {
				t.Parallel()
				xprSplitToMaster(t, g, ts, alias)
			})
		}
	}
}

func xprSplitToMaster(t *testing.T, g xprGroup, ts bool, alias sidebandGolden) {
	m := newLoopMaster(t)
	c := startEmbeddedLCClient(t, m, ts)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 50000}
	splits := ipsc.NewCallSplits()
	voice := 0
	first, _ := ipscpkt.DecodeUser(g.packets[0])
	for _, p := range g.packets {
		op, p, ok := splits.Repair(p[0], slices.Clone(p))
		if !ok {
			t.Fatalf("split packet %x dropped", p)
		}
		if u, _ := ipscpkt.DecodeUser(p); u.IsVoice() {
			voice++
		}
		c.HandleIPSCBurst(op, p, addr)
	}
	settle(t, func() int { return len(m.rawPackets()) }, 300*time.Millisecond)

	var side [][]byte
	var stream uint32
	gotVoice := 0
	for i, p := range m.rawPackets() {
		if !bytes.HasPrefix(p, []byte("DMRD")) {
			side = append(side, p)
			continue
		}
		d, err := dmrd.DecodeData(p)
		if err != nil {
			t.Fatal(err)
		}
		if stream == 0 {
			stream = d.Stream
		}
		if d.Source != first.Source || d.Destination != first.Destination || d.Stream != stream {
			t.Fatalf("DMRD %d: %d>%d stream %d, want %d>%d stream %d", i, d.Source, d.Destination, d.Stream, first.Source, first.Destination, stream)
		}
		if d.Frame == dmrd.FrameVoice || d.Frame == dmrd.FrameVoiceSync {
			gotVoice++
		}
	}
	if gotVoice != voice {
		t.Fatalf("master got %d voice DMRD, the XPR sent %d voice bursts", gotVoice, voice)
	}
	if len(side) != 1 || !bytes.Equal(side[0], alias.direct) {
		t.Fatalf("master got sideband %x, MMDVMHost sends %x", side, alias.direct)
	}
	if len(side[0]) != 19 {
		t.Fatalf("DMRA of %d octets, want 19", len(side[0]))
	}
	if body := scrapeMetrics(t, c.metrics); strings.Contains(body, `reason="sideband_busy"} 1`) {
		t.Fatal("the sideband was dropped as busy")
	}
}
