package ipsc

import (
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
)

func TestTerminatorDataLCReachesIPSCUnchanged(t *testing.T) {
	t.Parallel()
	tdlc := [9]byte{0x30, 0x00, 0x2F, 0xEB, 0xF6, 0x30, 0xB4, 0x3C, 0x60}
	lcb := pdu.LCBytes(tdlc)
	cw, err := lcb.Codeword(enums.DataTypeTerminatorWithLC)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := layer2.BuildDataBurst(cw, enums.DataTypeTerminatorWithLC, 7, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	for _, rewritten := range []bool{false, true} {
		tr := newTestTranslator(t)
		tr.SetColorCode(7)
		pkt := dmrd.Data{Source: 3191868, Destination: 3140598, Private: true, Slot: enums.Slot2,
			Frame: dmrd.FrameDataSync, Value: uint8(enums.DataTypeTerminatorWithLC), Stream: 77, Burst: raw}
		want := tdlc
		if rewritten {
			pkt.Destination = 3140599
			want[4] = 0xF7
		}
		out := tr.TranslateToIPSC(pkt)
		if len(out) != 1 {
			t.Fatalf("rewritten %v: got %d IPSC packets, want 1", rewritten, len(out))
		}
		u, err := ipscpkt.DecodeUser(out[0])
		if err != nil {
			t.Fatal(err)
		}
		if u.Opcode != ipscpkt.OpPrivateData || byte(u.Burst) != ipscBurstVoiceTerm || !u.Info.End() {
			t.Fatalf("rewritten %v: opcode %v burst %02x end %v, want a private data packet that ends the call", rewritten, u.Opcode, byte(u.Burst), u.Info.End())
		}
		if got := [9]byte(u.Data.Octets[:9]); got != want || !layer2.MMDVMHostAccepts(enums.DataTypeTerminatorWithLC, [12]byte(u.Data.Octets[:12])) {
			t.Fatalf("rewritten %v: LC %x, want %x with good parity", rewritten, u.Data.Octets[:12], want)
		}
	}
}
