package ipsc

import (
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
)

func TestMBCContinuationWithoutCRCReachesIPSC(t *testing.T) {
	t.Parallel()
	head := [12]byte{0x28, 0x00, 0x80, 0x30, 0xB4, 0x3C, 0x2F, 0xEB, 0xF6, 0x01}
	if err := pdu.Seal(&head, enums.DataTypeMBCHeader); err != nil {
		t.Fatal(err)
	}
	cont := [12]byte{0x44, 0x4D, 0x52, 0x47, 0x4F, 0x20, 0x54, 0x45, 0x53, 0x54, 0x00, 0x01}
	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	for _, b := range []struct {
		dt enums.DataType
		p  [12]byte
	}{{enums.DataTypeMBCHeader, head}, {enums.DataTypeMBCContinuation, cont}} {
		raw, err := layer2.BuildDataBurst(b.p, b.dt, 7, layer2.SyncSourceBS)
		if err != nil {
			t.Fatal(err)
		}
		pkt := makeTestMMDVMPacket(true, false, dmrd.FrameDataSync, uint8(b.dt))
		pkt.Burst = raw
		out := tr.TranslateToIPSC(pkt)
		if len(out) != 1 {
			t.Fatalf("%v: got %d IPSC packets, want 1", b.dt, len(out))
		}
		u, err := ipscpkt.DecodeUser(out[0])
		if err != nil {
			t.Fatal(err)
		}
		if byte(u.Burst) != byte(b.dt) || [12]byte(u.Data.Octets[:12]) != b.p {
			t.Fatalf("%v: burst %02x payload %x", b.dt, byte(u.Burst), u.Data.Octets[:12])
		}
	}
}
