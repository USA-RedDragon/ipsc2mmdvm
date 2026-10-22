package ipsc

import (
	"strings"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
)

func TestRawBurstsSendUndeliverableBursts(t *testing.T) {
	t.Parallel()
	for _, c := range undeliverableCases(t) {
		tr, _, m := newClockedTranslator(t)
		tr.SetColorCode(7)
		tr.SetRawBursts(true)
		pkt := makeTestMMDVMPacket(false, true, c.frame, uint8(c.dt))
		pkt.Burst = c.burst
		out := tr.TranslateToIPSC(pkt)
		if c.frame == dmrd.FrameReserved {
			if len(out) != 0 {
				t.Fatalf("%s: got %d IPSC packets, want none", c.name, len(out))
			}
			continue
		}
		if len(out) != 1 || out[0][30] != ipscBurstRaw {
			t.Fatalf("%s: got %x, want one 0x1F packet", c.name, out)
		}
		u, err := ipscpkt.DecodeUser(out[0])
		if err != nil {
			t.Fatal(err)
		}
		if u.Opcode != ipscpkt.OpPrivateData || u.Info.Slot() != enums.Slot2 || u.Data.Status != 0xC0 || u.Data.Words != 17 ||
			u.Data.Flags != [2]byte{0x80, 0x0A} || u.Data.Bits != 196 || len(out[0]) != 68 {
			t.Fatalf("%s: packet %x is not in the form of an XPR 8400", c.name, out[0])
		}
		raw, err := u.RawBurst(0, layer2.SyncSourceBS)
		if err != nil {
			t.Fatal(err)
		}
		if raw.Info() != c.burst.Info() {
			t.Fatalf("%s: info bits %x, want %x", c.name, raw.Info(), c.burst.Info())
		}
		if st := raw.SlotType(); st.DataType != c.dt || st.ColorCode != 7 {
			t.Fatalf("%s: slot type %+v", c.name, st)
		}
		if body := scrapeTranslatorMetrics(t, m); containsUndeliverable(body) {
			t.Fatalf("%s: counted as undeliverable", c.name)
		}
	}
}

func TestRawBurstsRoundTripXPRRawBlocks(t *testing.T) {
	t.Parallel()
	n := 0
	for _, g := range loadXPR8400Capture(t) {
		for _, r := range g.records {
			if r.data[0] != 0x83 && r.data[0] != 0x84 || len(r.data) != 68 || r.data[30] != ipscBurstRaw {
				continue
			}
			toMMDVM := newTestTranslator(t)
			toMMDVM.SetColorCode(7)
			pkts := toMMDVM.TranslateToMMDVM(r.data[0], r.data)
			if len(pkts) != 1 {
				t.Fatalf("%s: %x gave %d DMRD packets", g.name, r.data, len(pkts))
			}
			toIPSC := newTestTranslator(t)
			toIPSC.SetColorCode(7)
			toIPSC.SetRawBursts(true)
			out := toIPSC.TranslateToIPSC(pkts[0])
			if len(out) != 1 {
				t.Fatalf("%s: %x came back as %d IPSC packets", g.name, r.data, len(out))
			}
			got, want := out[0], r.data
			if got[0] != want[0] || got[17]&0x20 != want[17]&0x20 || got[30] != want[30] || string(got[31:65]) != string(want[31:65]) {
				t.Fatalf("%s: raw block\n got %x\nwant %x", g.name, got, want)
			}
			if got[65]&0x0F != want[65]&0x0F {
				t.Fatalf("%s: slot type %02x, want data type of %02x", g.name, got[65], want[65])
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("capture has no 68-octet raw blocks")
	}
}

func containsUndeliverable(body string) bool {
	return strings.Contains(body, "\ntranslator_undeliverable_total{")
}
