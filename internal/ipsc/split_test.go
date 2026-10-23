package ipsc

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/klauspost/compress/zstd"
)

func splitAliasLC() [9]byte {
	return [9]byte{0x04, 0x00, 0x76, 'K', 'I', '5', 'V', 'M', 'F'}
}

const (
	splitBogusSource      = 5655878
	splitBogusDestination = 0x4B4935
)

type xprSplit struct {
	packets   [][]byte
	src, dst  uint32
	voice     int
	sidebands int
}

func xprWholeCall(t *testing.T) [][]byte {
	t.Helper()
	var best [][]byte
	bestSync := 0
	for _, g := range loadXPR8400Capture(t) {
		calls := map[uint32][][]byte{}
		syncs := map[uint32]int{}
		for _, r := range g.records {
			if r.data[0] != 0x80 && r.data[0] != 0x81 {
				continue
			}
			u, err := ipscpkt.DecodeUser(r.data)
			if err != nil {
				continue
			}
			switch {
			case byte(u.Burst) == ipscBurstVoiceHead && len(calls[u.CallControl]) > 0 && syncs[u.CallControl] == 0:
				calls[u.CallControl] = append(calls[u.CallControl], r.data)
			case byte(u.Burst) == ipscBurstVoiceHead:
				calls[u.CallControl], syncs[u.CallControl] = [][]byte{r.data}, 0
			case len(calls[u.CallControl]) == 0:
			case u.IsVoice():
				calls[u.CallControl] = append(calls[u.CallControl], r.data)
				if u.Voice.Layout == ipscpkt.VoiceLayoutA {
					syncs[u.CallControl]++
				}
			case byte(u.Burst) == ipscBurstVoiceTerm:
				if n := syncs[u.CallControl]; n > bestSync {
					best, bestSync = append(slices.Clone(calls[u.CallControl]), r.data), n
				}
				delete(calls, u.CallControl)
			}
		}
	}
	if bestSync < 3 {
		t.Fatalf("longest XPR call has %d superframes", bestSync)
	}
	return best
}

func splitEdit(t *testing.T, p []byte, edit func(u *ipscpkt.User)) []byte {
	t.Helper()
	u, err := ipscpkt.DecodeUser(p)
	if err != nil {
		t.Fatal(err)
	}
	edit(&u)
	b, err := u.AppendBinary(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func buildXPRSplit(t *testing.T, call [][]byte, header bool) xprSplit {
	t.Helper()
	var sync []int
	for i, p := range call {
		u, _ := ipscpkt.DecodeUser(p)
		if u.IsVoice() && u.Voice.Layout == ipscpkt.VoiceLayoutA {
			sync = append(sync, i)
		}
	}
	a := sync[1]
	alias := splitAliasLC()
	frags := layer2.EncodeEmbeddedLC(alias)
	for j := range 4 {
		call[a+1+j] = splitEdit(t, call[a+1+j], func(u *ipscpkt.User) {
			u.Voice.Embedded = frags[j]
			if u.Voice.Layout == ipscpkt.VoiceLayoutE {
				u.Voice.LC = alias
			}
		})
	}
	u0, _ := ipscpkt.DecodeUser(call[0])
	s := xprSplit{src: u0.Source, dst: u0.Destination}
	bogus := func(p []byte) []byte {
		return splitEdit(t, p, func(u *ipscpkt.User) {
			if err := u.SetCall(true, splitBogusDestination, splitBogusSource); err != nil {
				t.Fatal(err)
			}
			u.CallControl += 0x1000
			u.CallSeq++
			u.RTP.Seq += 0x4000
			u.RTP.Timestamp += 0x40000000
		})
	}
	s.packets = append(s.packets, call[:a+4]...)
	if header {
		cw := fullLCCodeword(alias, enums.DataTypeVoiceLCHeader)
		for _, at := range []int{a + 4, a + 5} {
			rtp := u0.RTP
			ref, _ := ipscpkt.DecodeUser(call[at])
			rtp.Seq, rtp.Timestamp = ref.RTP.Seq, ref.RTP.Timestamp
			s.packets = append(s.packets, bogus(splitEdit(t, call[0], func(u *ipscpkt.User) {
				copy(u.Data.Octets[:12], cw[:])
				u.RTP = rtp
			})))
		}
		s.sidebands = 1
	} else {
		s.packets = append(s.packets, bogus(call[a+4]))
		s.sidebands = 1
	}
	for _, p := range call[a+6:] {
		s.packets = append(s.packets, bogus(p))
	}
	for _, p := range s.packets {
		if u, _ := ipscpkt.DecodeUser(p); u.IsVoice() {
			s.voice++
		}
	}
	return s
}

func repairAll(t *testing.T, packets [][]byte) [][]byte {
	t.Helper()
	cs := NewCallSplits()
	out := make([][]byte, 0, len(packets))
	for i, p := range packets {
		op, b, ok := cs.Repair(p[0], slices.Clone(p))
		if !ok {
			t.Fatalf("packet %d dropped", i)
		}
		if op != b[0] {
			t.Fatalf("packet %d: opcode 0x%02X for a packet of 0x%02X", i, op, b[0])
		}
		out = append(out, b)
	}
	return out
}

func TestCallSplitsLeaveTheCaptureAsItIs(t *testing.T) {
	t.Parallel()
	for _, g := range loadXPR8400Capture(t) {
		if len(g.records) == 0 {
			continue
		}
		cs := NewCallSplits()
		now := g.records[0].at
		cs.now = func() time.Time { return now }
		for i, r := range g.records {
			now = r.at
			op, b, ok := cs.Repair(r.data[0], r.data)
			if !ok || op != r.data[0] || !bytes.Equal(b, r.data) {
				t.Fatalf("%s packet %d: %v 0x%02X %x, want %x", g.name, i, ok, op, b, r.data)
			}
		}
	}
}

func TestCallSplitsJoinAnXPRSplitCall(t *testing.T) {
	t.Parallel()
	for _, header := range []bool{true, false} {
		s := buildXPRSplit(t, xprWholeCall(t), header)
		out := repairAll(t, s.packets)
		first, _ := ipscpkt.DecodeUser(out[0])
		prev := first.RTP
		for i, p := range out {
			u, err := ipscpkt.DecodeUser(p)
			if err != nil {
				t.Fatal(err)
			}
			if u.Source != s.src || u.Destination != s.dst || u.Opcode != first.Opcode || u.CallControl != first.CallControl {
				t.Fatalf("header %v packet %d: call %d>%d op %v cc %d, want %d>%d op %v cc %d", header, i,
					u.Source, u.Destination, u.Opcode, u.CallControl, s.src, s.dst, first.Opcode, first.CallControl)
			}
			if i > 0 && (u.RTP.Seq-prev.Seq > 2 || u.RTP.Timestamp-prev.Timestamp > 2*rtpVoiceStep) {
				t.Fatalf("header %v packet %d: RTP %d/%d after %d/%d", header, i, u.RTP.Seq, u.RTP.Timestamp, prev.Seq, prev.Timestamp)
			}
			prev = u.RTP
		}
	}
}

func TestCallSplitsDropASplitCallWithNoCallBeforeIt(t *testing.T) {
	t.Parallel()
	s := buildXPRSplit(t, xprWholeCall(t), true)
	cs := NewCallSplits()
	var sent int
	for _, p := range s.packets {
		u, _ := ipscpkt.DecodeUser(p)
		if u.Source != splitBogusSource {
			continue
		}
		if _, _, ok := cs.Repair(p[0], p); ok {
			sent++
		}
	}
	if sent != 0 {
		t.Fatalf("%d packets of a split call with no call before it went on", sent)
	}
}

func TestSplitCallToMMDVM(t *testing.T) {
	t.Parallel()
	for _, header := range []bool{true, false} {
		s := buildXPRSplit(t, xprWholeCall(t), header)
		tr := newTestTranslator(t)
		tr.SetColorCode(7)
		var got []dmrd.Data
		var lcs []EmbeddedLC
		for _, p := range repairAll(t, s.packets) {
			pkts, lc, ok := tr.TranslateToMMDVMWithLC(p[0], p)
			got = append(got, pkts...)
			if ok {
				lcs = append(lcs, lc)
			}
		}
		voice, headers := 0, 0
		for i, p := range got {
			if p.Source != s.src || p.Destination != s.dst || p.Stream != got[0].Stream {
				t.Fatalf("header %v DMRD %d: %d>%d stream %d, want %d>%d stream %d", header, i,
					p.Source, p.Destination, p.Stream, s.src, s.dst, got[0].Stream)
			}
			switch {
			case p.Frame == dmrd.FrameVoice || p.Frame == dmrd.FrameVoiceSync:
				voice++
			case p.Value == uint8(enums.DataTypeVoiceLCHeader):
				headers++
			}
		}
		if voice != s.voice || headers != 1 {
			t.Fatalf("header %v: %d voice DMRD and %d voice headers, want %d and 1", header, voice, headers, s.voice)
		}
		if len(lcs) != s.sidebands {
			t.Fatalf("header %v: %d sideband LCs, want %d", header, len(lcs), s.sidebands)
		}
		for _, lc := range lcs {
			b, err := lc.LC.Bytes()
			if err != nil || b != splitAliasLC() || lc.Source != s.src {
				t.Fatalf("header %v: LC %x from %d, want %x from %d", header, b, lc.Source, splitAliasLC(), s.src)
			}
		}
	}
}

func TestTalkerAliasAndGPSHeaderPacketsAreNotVoiceHeaders(t *testing.T) {
	t.Parallel()
	call := xprWholeCall(t)
	for _, lc := range [][9]byte{splitAliasLC(), {0x05, 0x00, 'i', 'p', 's', 'c', '2', 'h', 'b'}, testGPSLC()} {
		cw := fullLCCodeword(lc, enums.DataTypeVoiceLCHeader)
		tr := newTestTranslator(t)
		tr.SetColorCode(7)
		var headers int
		var lcs []EmbeddedLC
		for i := range 3 {
			p := call[0]
			if i > 0 {
				p = splitEdit(t, p, func(u *ipscpkt.User) { copy(u.Data.Octets[:12], cw[:]) })
			}
			pkts, got, ok := tr.TranslateToMMDVMWithLC(p[0], p)
			headers += len(pkts)
			if ok {
				lcs = append(lcs, got)
			}
		}
		if headers != 1 || len(lcs) != 1 {
			t.Fatalf("LC %x: %d DMRD and %d sideband LCs, want 1 and 1", lc, headers, len(lcs))
		}
		u, _ := ipscpkt.DecodeUser(call[0])
		b, _ := lcs[0].LC.Bytes()
		c := lcs[0].Call
		if b != lc || lcs[0].Source != u.Source || c.Source != u.Source || c.Destination != u.Destination || c.Private || c.Burst != (layer2.RawBurst{}) {
			t.Fatalf("LC %x: sideband %x from %d with call %+v", lc, b, lcs[0].Source, c)
		}
	}
}

func TestBurstEThatStartsACallGivesItsLC(t *testing.T) {
	t.Parallel()
	s := buildXPRSplit(t, xprWholeCall(t), false)
	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	var lcs []EmbeddedLC
	for _, p := range s.packets {
		if u, _ := ipscpkt.DecodeUser(p); u.Source != splitBogusSource {
			continue
		}
		if pkts, lc, ok := tr.TranslateToMMDVMWithLC(p[0], p); ok {
			if len(lcs) == 0 && (len(pkts) == 0 || pkts[len(pkts)-1].Value != 4) {
				t.Fatalf("LC with %+v, want voice burst E", pkts)
			}
			lcs = append(lcs, lc)
		}
	}
	if len(lcs) != 1 {
		t.Fatalf("%d sideband LCs, want 1", len(lcs))
	}
	if b, _ := lcs[0].LC.Bytes(); b != splitAliasLC() {
		t.Fatalf("LC %x, want %x", b, splitAliasLC())
	}
}

func renderXPRSplits(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	at := time.Date(0, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, header := range []bool{true, false} {
		name := "split at burst E"
		if header {
			name = "split at voice LC headers"
		}
		fmt.Fprintf(&b, "# %s\n", name)
		for _, p := range buildXPRSplit(t, xprWholeCall(t), header).packets {
			fmt.Fprintf(&b, "%s %s\n", at.Format("15:04:05.000"), hex.EncodeToString(p))
			at = at.Add(60 * time.Millisecond)
		}
	}
	return b.String()
}

func TestXPRSplitCapture(t *testing.T) {
	t.Parallel()
	const path = "testdata/xpr8400-split-ipsc.txt.zst"
	want := renderXPRSplits(t)
	if os.Getenv("UPDATE_SPLIT") != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, want); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s is not what buildXPRSplit makes; run UPDATE_SPLIT=1 go test -run TestXPRSplitCapture", path)
	}
}

func TestServerJoinsSplitCallsBeforeTheHandler(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	m := metrics.NewMetrics()
	s.metrics = m
	type got struct {
		op   byte
		data []byte
	}
	ch := make(chan got, 64)
	s.SetBurstHandler(func(op byte, data []byte, _ *net.UDPAddr) { ch <- got{op, data} })
	startBurstWorker(t, s)
	split := buildXPRSplit(t, xprWholeCall(t), true)
	var orphan [][]byte
	for _, p := range split.packets {
		if u, _ := ipscpkt.DecodeUser(p); u.Source == splitBogusSource && len(orphan) < 2 {
			orphan = append(orphan, splitEdit(t, p, func(u *ipscpkt.User) { u.CallControl += 0x1000 }))
		}
	}
	for _, p := range append(orphan, split.packets...) {
		s.bursts <- burst{packetType: p[0], data: p}
	}
	for i, want := range repairAll(t, split.packets) {
		g := <-ch
		if g.op != want[0] || !bytes.Equal(g.data, want) {
			t.Fatalf("packet %d: handler got 0x%02X %x, want %x", i, g.op, g.data, want)
		}
	}
	if n := dropped(t, m, "xpr_split"); n != float64(len(orphan)) {
		t.Fatalf("%v packets counted as xpr_split, want %d", n, len(orphan))
	}
}

func syncLostAfter(t *testing.T, term []byte, n uint16) []byte {
	t.Helper()
	return splitEdit(t, term, func(u *ipscpkt.User) {
		u.Burst = ipscpkt.BurstSyncLost
		u.RTP.Seq += n
		u.RTP.Timestamp += uint32(n) * rtpVoiceStep
	})
}

func TestCallSplitsRepairSyncLostAfterTheTerminator(t *testing.T) {
	t.Parallel()
	s := buildXPRSplit(t, xprWholeCall(t), false)
	term := s.packets[len(s.packets)-1]
	packets := append(slices.Clone(s.packets), syncLostAfter(t, term, 1), syncLostAfter(t, term, 2))
	for i, p := range repairAll(t, packets) {
		if u, _ := ipscpkt.DecodeUser(p); u.Source != s.src || u.Destination != s.dst {
			t.Fatalf("packet %d: %d>%d, want %d>%d", i, u.Source, u.Destination, s.src, s.dst)
		}
	}
}

func TestCallSplitsDropASplitAfterTheCallEnded(t *testing.T) {
	t.Parallel()
	s := buildXPRSplit(t, xprWholeCall(t), true)
	cs := NewCallSplits()
	var parentTerm []byte
	for _, p := range s.packets {
		u, _ := ipscpkt.DecodeUser(p)
		if u.Source == splitBogusSource {
			break
		}
		parentTerm = p
		if _, _, ok := cs.Repair(p[0], p); !ok {
			t.Fatal("packet of the first call dropped")
		}
	}
	end := splitEdit(t, parentTerm, func(u *ipscpkt.User) {
		u.Burst = ipscpkt.BurstType(ipscBurstVoiceTerm)
		u.RTP.Seq++
	})
	for _, p := range [][]byte{end, syncLostAfter(t, end, 1), syncLostAfter(t, end, 2)} {
		if _, _, ok := cs.Repair(p[0], p); !ok {
			t.Fatal("terminator of the first call dropped")
		}
	}
	for _, p := range s.packets {
		if u, _ := ipscpkt.DecodeUser(p); u.Source != splitBogusSource {
			continue
		}
		if _, b, ok := cs.Repair(p[0], p); ok {
			t.Fatalf("%x of a split after the call ended went on", b)
		}
	}
}

func TestCallSplitsJoinWhenTheFirstPacketIsLost(t *testing.T) {
	t.Parallel()
	s := buildXPRSplit(t, xprWholeCall(t), false)
	alias := splitAliasLC()
	var packets [][]byte
	lost, late := false, -1
	for _, p := range s.packets {
		u, _ := ipscpkt.DecodeUser(p)
		if u.Source == splitBogusSource && !lost {
			lost = true
			continue
		}
		if lost && late < 0 && u.IsVoice() && u.Voice.Layout == ipscpkt.VoiceLayoutE {
			late = len(packets)
			p = splitEdit(t, p, func(u *ipscpkt.User) { u.Voice.LC = alias })
		}
		packets = append(packets, p)
	}
	for i, p := range repairAll(t, packets) {
		if i < late {
			continue
		}
		if u, _ := ipscpkt.DecodeUser(p); u.Source != s.src || u.Destination != s.dst {
			t.Fatalf("packet %d: %d>%d, want %d>%d", i, u.Source, u.Destination, s.src, s.dst)
		}
	}
}

func TestCallSplitsKeepPeersApart(t *testing.T) {
	t.Parallel()
	s := buildXPRSplit(t, xprWholeCall(t), true)
	other := func(p []byte) []byte {
		return splitEdit(t, p, func(u *ipscpkt.User) { u.ID++ })
	}
	cs := NewCallSplits()
	for i, p := range s.packets {
		if _, _, ok := cs.Repair(p[0], p); !ok {
			t.Fatalf("packet %d dropped", i)
		}
		u, _ := ipscpkt.DecodeUser(p)
		if u.Source != splitBogusSource {
			continue
		}
		q := other(splitEdit(t, s.packets[0], func(v *ipscpkt.User) { v.CallControl = u.CallControl }))
		if op, b, ok := cs.Repair(q[0], q); !ok || op != q[0] || !bytes.Equal(b, q) {
			t.Fatalf("packet of another peer on call control %d: %v %x, want %x", u.CallControl, ok, b, q)
		}
	}
}
