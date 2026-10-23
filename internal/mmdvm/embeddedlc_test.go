package mmdvm

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

type sidebandGolden struct {
	lc      [9]byte
	direct  []byte
	gateway []byte
}

func loadSidebandGolden(t *testing.T) (source uint32, rows []sidebandGolden) {
	t.Helper()
	f, err := os.Open("testdata/sideband/mmdvmhost.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "# source "); ok {
			b, err := hex.DecodeString(v)
			if err != nil || len(b) != 3 {
				t.Fatalf("bad source line %q", line)
			}
			source = uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 {
			t.Fatalf("bad golden line %q", line)
		}
		var r sidebandGolden
		lc, err := hex.DecodeString(f[0])
		if err != nil || len(lc) != 9 {
			t.Fatalf("bad LC %q", f[0])
		}
		r.lc = [9]byte(lc)
		if r.direct, err = hex.DecodeString(f[1]); err != nil {
			t.Fatal(err)
		}
		if r.gateway, err = hex.DecodeString(f[3]); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if source == 0 || len(rows) == 0 {
		t.Fatal("empty sideband golden")
	}
	return source, rows
}

func longestXPRCall(t *testing.T) [][]byte {
	t.Helper()
	var best [][]byte
	bestSync := 0
	for _, g := range loadXPRGroups(t) {
		calls := map[uint32][][]byte{}
		syncs := map[uint32]int{}
		var order []uint32
		for _, p := range g.packets {
			u, err := ipscpkt.DecodeUser(p)
			if err != nil || u.Opcode != ipscpkt.OpGroupVoice && u.Opcode != ipscpkt.OpPrivateVoice {
				continue
			}
			if _, ok := calls[u.CallControl]; !ok {
				order = append(order, u.CallControl)
			}
			calls[u.CallControl] = append(calls[u.CallControl], p)
			if u.IsVoice() && u.Voice.Layout == ipscpkt.VoiceLayoutA {
				syncs[u.CallControl]++
			}
		}
		for _, cc := range order {
			if n := syncs[cc]; n > bestSync {
				best, bestSync = calls[cc], n
			}
		}
	}
	return best
}

func xprCallWithEmbeddedLCs(t *testing.T, lcs [][9]byte) (packets [][]byte, burstE []int) {
	t.Helper()
	packets = longestXPRCall(t)
	var sync []int
	for i, p := range packets {
		u, err := ipscpkt.DecodeUser(p)
		if err == nil && u.IsVoice() && u.Voice.Layout == ipscpkt.VoiceLayoutA {
			sync = append(sync, i)
		}
	}
	if len(sync) < len(lcs)+1 {
		t.Fatalf("longest XPR call has %d superframes, want %d", len(sync), len(lcs)+1)
	}
	for k, lc := range lcs {
		a := sync[k+1]
		frags := layer2.EncodeEmbeddedLC(lc)
		for j := range 4 {
			u, err := ipscpkt.DecodeUser(packets[a+1+j])
			if err != nil || !u.IsVoice() || (j == 3) != (u.Voice.Layout == ipscpkt.VoiceLayoutE) {
				t.Fatalf("packet %d is not voice burst %c", a+1+j, 'B'+j)
			}
			u.Voice.Embedded = frags[j]
			if j == 3 {
				u.Voice.LC = lc
			}
			b, err := u.AppendBinary(nil)
			if err != nil {
				t.Fatal(err)
			}
			packets[a+1+j] = b
		}
		burstE = append(burstE, a+4)
	}
	return packets, burstE
}

func TestEmbeddedLCToMasterMatchesMMDVMHost(t *testing.T) {
	t.Parallel()
	for _, ts := range []bool{false, true} {
		name := "direct"
		if ts {
			name = "timeslot manager"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			embeddedLCToMaster(t, ts)
		})
	}
}

func startEmbeddedLCClient(t *testing.T, m *loopMaster, ts bool) *MMDVMClient {
	t.Helper()
	c, _, _ := startLoopClient(t, m, false)
	if ts {
		c.inboundTSMgr = timeslot.NewManager()
	}
	return c
}

func embeddedLCToMaster(t *testing.T, ts bool) {
	source, golden := loadSidebandGolden(t)
	lcs := make([][9]byte, 0, len(golden))
	for _, g := range golden {
		lcs = append(lcs, g.lc)
	}
	packets, _ := xprCallWithEmbeddedLCs(t, lcs)
	if u, err := ipscpkt.DecodeUser(packets[0]); err != nil || u.Source != source {
		t.Fatalf("XPR call source %d, golden source %d", u.Source, source)
	}

	m := newLoopMaster(t)
	c := startEmbeddedLCClient(t, m, ts)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 50000}
	for _, p := range packets {
		c.HandleIPSCBurst(p[0], slices.Clone(p), addr)
	}
	settle(t, func() int { return len(m.rawPackets()) }, 300*time.Millisecond)

	raw := m.rawPackets()
	var side [][]byte
	for i, p := range raw {
		if !bytes.HasPrefix(p, []byte("DMRA")) && !bytes.HasPrefix(p, []byte("DMRG")) {
			continue
		}
		side = append(side, p)
		if i+1 >= len(raw) {
			t.Fatalf("%x is the last packet to the master", p)
		}
		next, err := dmrd.DecodeData(raw[i+1])
		if err != nil || next.Frame != dmrd.FrameVoice || next.Value != 4 {
			t.Fatalf("%x is not followed by the DMRD of voice burst E: %x", p, raw[i+1])
		}
	}
	if len(side) != len(golden) {
		t.Fatalf("master got %d DMRA and DMRG packets, want %d", len(side), len(golden))
	}
	for i, g := range golden {
		if !bytes.Equal(side[i], g.direct) {
			t.Errorf("packet %d: %x, MMDVMHost sends %x", i, side[i], g.direct)
		}
		if !bytes.HasPrefix(side[i], g.gateway) {
			t.Errorf("packet %d: %x does not start with the DMRGateway form %x", i, side[i], g.gateway)
		}
		if bytes.HasPrefix(side[i], []byte("DMRA")) {
			ta, err := dmrd.DecodeTalkerAlias(side[i])
			if err != nil || ta.Size != 19 || ta.Repeater != c.cfg.ID || ta.Source != source {
				t.Errorf("packet %d: DMRA %+v, %v", i, ta, err)
			}
			continue
		}
		pos, err := dmrd.DecodePosition(side[i])
		if err != nil || pos.Size != 18 || pos.Repeater != c.cfg.ID || pos.Source != source {
			t.Errorf("packet %d: DMRG %+v, %v", i, pos, err)
		}
	}

	plain := newLoopMaster(t)
	pc := startEmbeddedLCClient(t, plain, ts)
	for _, p := range longestXPRCall(t) {
		pc.HandleIPSCBurst(p[0], slices.Clone(p), addr)
	}
	settle(t, func() int { return len(plain.rawPackets()) }, 300*time.Millisecond)
	if n, want := len(raw)-len(side), len(plain.rawPackets()); n != want {
		t.Fatalf("master got %d DMRD packets with the embedded LCs, %d without", n, want)
	}
}

func TestEmbeddedLCNeverWaitsForRoom(t *testing.T) {
	t.Parallel()
	_, golden := loadSidebandGolden(t)
	packets, burstE := xprCallWithEmbeddedLCs(t, [][9]byte{golden[0].lc})
	c := newTestClient(t)
	c.metrics = metrics.NewMetrics()
	c.started.Store(true)
	c.inboundTSMgr = nil
	c.cfg.PassAllTG = []int{1, 2}
	c.cfg.PassAllPC = []int{1, 2}
	c.buildRewriteRules()
	c.translator.SetPeerID(c.cfg.ID)
	c.translator.SetColorCode(7)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 50000}
	for _, p := range packets[:burstE[0]] {
		c.HandleIPSCBurst(p[0], slices.Clone(p), addr)
		for len(c.tx_chan) > 0 {
			<-c.tx_chan
		}
	}
	for len(c.tx_chan) < cap(c.tx_chan)-1 {
		c.tx_chan <- toMaster{}
	}
	done := make(chan struct{})
	go func() {
		c.HandleIPSCBurst(packets[burstE[0]][0], slices.Clone(packets[burstE[0]]), addr)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the talker alias took the room of voice burst E")
	}
	for len(c.tx_chan) > 1 {
		<-c.tx_chan
	}
	if p := <-c.tx_chan; p.side != nil || p.Frame != dmrd.FrameVoice || p.Value != 4 {
		t.Fatalf("last queued packet %+v, want voice burst E", p)
	}
	want := `mmdvm_packets_dropped_total{network="` + c.cfg.Name + `",reason="sideband_busy"} 1`
	if body := scrapeMetrics(t, c.metrics); !strings.Contains(body, want) {
		t.Fatalf("metrics have no %s:\n%s", want, grepLines(body, "mmdvm_packets_dropped_total"))
	}
}
