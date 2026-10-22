package ipsc

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRawIPSCBlocksReachMMDVM(t *testing.T) {
	t.Parallel()
	seen := map[enums.DataType]int{}
	for _, g := range loadXPR8400Capture(t) {
		for _, r := range g.records {
			if r.data[0] != 0x83 && r.data[0] != 0x84 || len(r.data) <= 30 || r.data[30] != byte(ipscpkt.BurstRaw) {
				continue
			}
			u, err := ipscpkt.DecodeUser(r.data)
			if err != nil {
				t.Fatal(err)
			}
			st, ok := u.Data.SlotType()
			if !ok {
				t.Fatalf("%s: 0x1F packet %x has no slot type", g.name, r.data)
			}
			tr := newTestTranslator(t)
			tr.SetColorCode(7)
			out := tr.TranslateToMMDVM(r.data[0], r.data)
			if len(out) != 1 {
				t.Fatalf("%s: 0x1F packet %x gave %d DMRD packets, want 1", g.name, r.data, len(out))
			}
			pkt := out[0]
			if pkt.Frame != dmrd.FrameDataSync || enums.DataType(pkt.Value) != st.DataType() {
				t.Fatalf("%s: frame %v value %d, want data sync %v", g.name, pkt.Frame, pkt.Value, st.DataType())
			}
			info := pkt.Burst.Info()
			want := [25]byte(u.Data.Octets[:25])
			want[24] &= 0xF0
			if info != want {
				t.Fatalf("%s: info bits %x, want %x", g.name, info, want)
			}
			gotST := pkt.Burst.SlotType()
			if gotST.FEC.Uncorrectable || gotST.ColorCode != 7 || gotST.DataType != st.DataType() {
				t.Fatalf("%s: slot type %+v", g.name, gotST)
			}
			if p, _ := pkt.Burst.Sync(); p != enums.SyncBSSourcedData {
				t.Fatalf("%s: sync %v", g.name, p)
			}
			seen[st.DataType()]++
		}
	}
	for _, dt := range []enums.DataType{enums.DataTypeRate1, enums.DataTypeUnifiedSingleBlock} {
		if seen[dt] == 0 {
			t.Errorf("capture has no 0x1F packet of %v", dt)
		}
	}
}

func TestIdleIPSCBurstReachesMMDVM(t *testing.T) {
	t.Parallel()
	p := mustHex(t, "840004ce2e552febf630b43c0100007f1b00805d31d06224376d000000000940000a800a0060bd00000130b43c2febf621a600495c89")
	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	out := tr.TranslateToMMDVM(p[0], p)
	if len(out) != 1 {
		t.Fatalf("idle packet gave %d DMRD packets, want 1", len(out))
	}
	if out[0].Frame != dmrd.FrameDataSync || enums.DataType(out[0].Value) != enums.DataTypeIdle {
		t.Fatalf("frame %v value %d, want idle", out[0].Frame, out[0].Value)
	}
	payload, _ := out[0].Burst.BPTCPayload()
	if want := [12]byte(p[38:50]); payload != want {
		t.Fatalf("payload %x, want %x", payload, want)
	}
	if st := out[0].Burst.SlotType(); st.DataType != enums.DataTypeIdle || st.ColorCode != 7 {
		t.Fatalf("slot type %+v", st)
	}
}

func TestBadCRCIPSCBurstsReachMMDVM(t *testing.T) {
	t.Parallel()
	seen := map[enums.DataType]int{}
	for _, g := range loadXPR8400Capture(t) {
		for _, r := range g.records {
			if r.data[0] != 0x83 && r.data[0] != 0x84 || len(r.data) <= 30 {
				continue
			}
			dt := enums.DataType(r.data[30])
			if dt != enums.DataTypeCSBK && dt != enums.DataTypeDataHeader && dt != enums.DataTypeMBCHeader {
				continue
			}
			u, err := ipscpkt.DecodeUser(r.data)
			if err != nil || u.Data.Bits != 96 {
				continue
			}
			payload := [12]byte(u.Data.Octets[:12])
			if pdu.BlockCRCOK(payload, dt) {
				continue
			}
			tr := newTestTranslator(t)
			tr.SetColorCode(7)
			out := tr.TranslateToMMDVM(r.data[0], r.data)
			if len(out) != 1 {
				t.Fatalf("%s: %v with a bad CRC %x gave %d DMRD packets, want 1", g.name, dt, r.data, len(out))
			}
			if got, _ := out[0].Burst.BPTCPayload(); got != payload || enums.DataType(out[0].Value) != dt {
				t.Fatalf("%s: %v payload %x, want %v %x", g.name, enums.DataType(out[0].Value), got, dt, payload)
			}
			seen[dt]++
		}
	}
	if seen[enums.DataTypeCSBK] == 0 || seen[enums.DataTypeDataHeader] == 0 {
		t.Fatalf("capture has too few bad CRC bursts: %v", seen)
	}
}

func TestEveryIPSCDataBlockReachesMMDVM(t *testing.T) {
	t.Parallel()
	blocks := 0
	for _, g := range loadXPR8400Capture(t) {
		tr := newTestTranslator(t)
		tr.SetColorCode(7)
		var now time.Time
		tr.now = func() time.Time { return now }
		for i, r := range g.records {
			now = r.at
			if r.data[0] != 0x83 && r.data[0] != 0x84 || len(r.data) <= 30 {
				continue
			}
			bt := r.data[30]
			if bt != ipscBurstRate12 && bt != ipscBurstRate34 && bt != ipscBurstMBCContinuation {
				continue
			}
			out := tr.TranslateToMMDVM(r.data[0], r.data)
			if len(out) != 1 || out[0].Value != bt {
				t.Fatalf("%s packet %d: block %x gave %+v, want one DMRD of data type %d", g.name, i, r.data, out, bt)
			}
			blocks++
		}
	}
	if blocks == 0 {
		t.Fatal("capture has no data blocks")
	}
}

func TestHeaderlessMMDVMBlocksReachIPSC(t *testing.T) {
	t.Parallel()
	r12 := [12]byte{0x45, 0x00, 0x48, 0x00, 0x59, 0x00, 0x54, 0x00, 0x20, 0x00, 0x54, 0x00}
	r34 := [18]byte{0x00, 0x50, 0x44, 0x5c, 0x56, 0x47, 0x1f, 0x20, 0x54, 0x45, 0x53, 0x14, 0x20, 0x00, 0x00, 0x00, 0x00, 0x01}
	b12, err := layer2.BuildDataBurst(r12, enums.DataTypeRate12, 7, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	b34, err := layer2.BuildRate34DataBurst(r34, 7, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	for i, b := range []layer2.RawBurst{b12, b34, b12} {
		dt := enums.DataTypeRate12
		if i == 1 {
			dt = enums.DataTypeRate34
		}
		pkt := makeTestMMDVMPacket(false, false, dmrd.FrameDataSync, uint8(dt))
		pkt.Burst = b
		out := tr.TranslateToIPSC(pkt)
		if len(out) != 1 {
			t.Fatalf("block %d: %v with no header gave %d IPSC packets, want 1", i, dt, len(out))
		}
		u, err := ipscpkt.DecodeUser(out[0])
		if err != nil {
			t.Fatal(err)
		}
		raw, err := u.RawBurst(7, layer2.SyncSourceBS)
		if err != nil || byte(u.Burst) != byte(dt) || u.Info.End() {
			t.Fatalf("block %d: burst %02x end %v error %v", i, byte(u.Burst), u.Info.End(), err)
		}
		if dt == enums.DataTypeRate12 {
			if p, _ := raw.BPTCPayload(); p != r12 {
				t.Fatalf("block %d: payload %x", i, p)
			}
		} else if p, _ := raw.Rate34Payload(); p != r34 {
			t.Fatalf("block %d: payload %x", i, p)
		}
	}
	end := tr.EndDataCall(makeTestMMDVMPacket(false, false, dmrd.FrameDataSync, uint8(enums.DataTypeRate12)))
	if len(end) != 1 || end[0][30] != ipscBurstSyncUndetect || end[0][17]&ipscCallInfoEnd == 0 {
		t.Fatalf("end of the call gave %x", end)
	}
}
