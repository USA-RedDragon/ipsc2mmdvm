package ipsc

import (
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
)

func testTalkerAliasLC() [9]byte { return [9]byte{0x04, 0x00, 0x4E, 'K', 'I', '5', 'V', 'M', 'F'} }

func testGPSLC() [9]byte { return [9]byte{0x08, 0x00, 0x0B, 0x9A, 0x6C, 0x3E, 0x27, 0x91, 0x0D} }

func xprVoiceCallToMMDVM(t *testing.T) []dmrd.Data {
	t.Helper()
	var best []dmrd.Data
	for _, g := range loadXPR8400Capture(t) {
		if len(g.records) == 0 {
			continue
		}
		tr := newTestTranslator(t)
		tr.SetColorCode(7)
		now := g.records[0].at
		tr.now = func() time.Time { return now }
		var call []dmrd.Data
		for _, r := range g.records {
			now = r.at
			if r.data[0] != 0x80 && r.data[0] != 0x81 {
				continue
			}
			for _, p := range tr.TranslateToMMDVM(r.data[0], r.data) {
				if len(call) > 0 && p.Stream != call[0].Stream {
					call = nil
				}
				call = append(call, p)
				if isTerminator(p) {
					if len(superframes(call)) > len(superframes(best)) {
						best = call
					}
					call = nil
				}
			}
		}
	}
	if len(best) == 0 {
		t.Fatal("no voice call in the capture")
	}
	return best
}

func isTerminator(p dmrd.Data) bool {
	return p.Frame == dmrd.FrameDataSync && p.Value == 2
}

func superframes(call []dmrd.Data) [][]int {
	var sfs [][]int
	for i, p := range call {
		if p.Frame == dmrd.FrameVoiceSync {
			sfs = append(sfs, []int{i})
			continue
		}
		if p.Frame == dmrd.FrameVoice && len(sfs) > 0 {
			sfs[len(sfs)-1] = append(sfs[len(sfs)-1], i)
		}
	}
	var full [][]int
	for _, sf := range sfs {
		if len(sf) == 6 {
			full = append(full, sf)
		}
	}
	return full
}

func TestTalkerAliasAndGPSSurviveMMDVMToIPSCToMMDVM(t *testing.T) {
	t.Parallel()
	call := xprVoiceCallToMMDVM(t)
	sfs := superframes(call)
	if len(sfs) < 3 {
		t.Fatalf("call has %d full superframes", len(sfs))
	}
	want := map[int][4]byte{}
	for k, lc := range map[int][9]byte{1: testTalkerAliasLC(), 2: testGPSLC()} {
		frags := layer2.EncodeEmbeddedLC(lc)
		for j := range 4 {
			i := sfs[k][1+j]
			call[i].Burst.SetEmbeddedData(frags[j])
			want[i] = frags[j]
		}
	}
	toIPSC := newTestTranslator(t)
	toIPSC.SetColorCode(7)
	back := newTestTranslator(t)
	back.SetColorCode(7)
	ipscOut := make([][]byte, 0, len(call)+8)
	gotBack := make([]dmrd.Data, 0, len(call))
	for i, p := range call {
		out := toIPSC.TranslateToIPSC(p)
		if frag, ok := want[i]; ok {
			if len(out) != 1 {
				t.Fatalf("burst %d gave %d IPSC packets", i, len(out))
			}
			u, err := ipscpkt.DecodeUser(out[0])
			if err != nil {
				t.Fatal(err)
			}
			if u.Voice.Embedded != frag {
				t.Fatalf("burst %d: IPSC embedded %x, want %x", i, u.Voice.Embedded, frag)
			}
		}
		ipscOut = append(ipscOut, out...)
	}
	for _, p := range ipscOut {
		gotBack = append(gotBack, back.TranslateToMMDVM(p[0], p)...)
	}
	gotSFs := superframes(gotBack)
	if len(gotSFs) != len(sfs) {
		t.Fatalf("got %d superframes back, want %d", len(gotSFs), len(sfs))
	}
	for k, lc := range map[int][9]byte{1: testTalkerAliasLC(), 2: testGPSLC()} {
		var frags [4][4]byte
		for j := range 4 {
			b := gotBack[gotSFs[k][1+j]].Burst
			frags[j] = b.EmbeddedData()
			if emb := b.EMB(); emb.FEC.Uncorrectable {
				t.Fatalf("superframe %d burst %d: EMB does not decode", k, j)
			}
		}
		got, res := layer2.DecodeEmbeddedLC(frags)
		if res.Uncorrectable || got != lc {
			t.Fatalf("superframe %d: embedded LC %x, want %x", k, got, lc)
		}
	}
}

func TestTalkerAliasAndGPSSurviveIPSCToMMDVMToIPSC(t *testing.T) {
	t.Parallel()
	packets := longestXPRVoiceCall(t)
	var sync []int
	for i, p := range packets {
		u, err := ipscpkt.DecodeUser(p)
		if err == nil && u.IsVoice() && u.Voice.Layout == ipscpkt.VoiceLayoutA {
			sync = append(sync, i)
		}
	}
	want := map[int][9]byte{}
	for k, lc := range [][9]byte{testTalkerAliasLC(), testGPSLC()} {
		a := sync[k+1]
		frags := layer2.EncodeEmbeddedLC(lc)
		for j := range 4 {
			u, err := ipscpkt.DecodeUser(packets[a+1+j])
			if err != nil || !u.IsVoice() {
				t.Fatalf("packet %d is not a voice burst", a+1+j)
			}
			u.Voice.Embedded = frags[j]
			b, err := u.AppendBinary(nil)
			if err != nil {
				t.Fatal(err)
			}
			packets[a+1+j] = b
		}
		want[a] = lc
	}
	toMMDVM := newTestTranslator(t)
	toMMDVM.SetColorCode(7)
	toIPSC := newTestTranslator(t)
	toIPSC.SetColorCode(7)
	var out [][]byte
	for _, p := range packets {
		for _, d := range toMMDVM.TranslateToMMDVM(p[0], p) {
			out = append(out, toIPSC.TranslateToIPSC(d)...)
		}
	}
	found := 0
	var frags [][4]byte
	for _, p := range out {
		u, err := ipscpkt.DecodeUser(p)
		if err != nil || !u.IsVoice() {
			continue
		}
		if u.Voice.Layout == ipscpkt.VoiceLayoutA {
			frags = nil
			continue
		}
		frags = append(frags, u.Voice.Embedded)
		if len(frags) != 4 {
			continue
		}
		lc, res := layer2.DecodeEmbeddedLC([4][4]byte(frags))
		if !res.Uncorrectable && (lc == testTalkerAliasLC() || lc == testGPSLC()) {
			found++
		}
	}
	if found != len(want) {
		t.Fatalf("found %d of %d talker alias and GPS superframes after the round trip", found, len(want))
	}
}

func longestXPRVoiceCall(t *testing.T) [][]byte {
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
			calls[u.CallControl] = append(calls[u.CallControl], r.data)
			if u.IsVoice() && u.Voice.Layout == ipscpkt.VoiceLayoutA {
				syncs[u.CallControl]++
			}
		}
		for cc, n := range syncs {
			if n > bestSync {
				best, bestSync = calls[cc], n
			}
		}
	}
	if bestSync < 4 {
		t.Fatalf("longest voice call has %d superframes", bestSync)
	}
	return best
}
