package ipsc

import (
	"encoding/hex"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
)

// TestEndStreamsEndsOpenCalls opens the PI voice call of rigPICallBursts
// without its terminator, and the confirmed rate 3/4 packet of
// rigRate34Calls without its block, then ends both with EndStreams, as at
// shutdown. The voice call ends with the voice terminator that its own
// terminator burst gives, and the data call with the end packet an XPR 8400
// sends when it loses the signal.
func TestEndStreamsEndsOpenCalls(t *testing.T) {
	t.Parallel()
	pi := interleavedMMDVMCalls(t)[0]
	whole := newTestTranslator(t)
	whole.SetColorCode(7)
	var want []byte
	for _, p := range pi {
		if out := whole.TranslateToIPSC(p); len(out) > 0 {
			want = out[len(out)-1]
		}
	}

	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	for _, p := range pi[:len(pi)-1] {
		if len(tr.TranslateToIPSC(p)) == 0 {
			t.Fatalf("voice call packet %d/%d: no IPSC packets", p.Frame, p.Value)
		}
	}
	c := rigRate34Calls()[1]
	header, _ := hex.DecodeString(c.bursts[0])
	if out := tr.TranslateToIPSC(dmrd.Data{
		Source: 3191868, Destination: c.dst, Private: !c.group, Slot: enums.Slot2,
		Frame: dmrd.FrameDataSync, Value: uint8(c.types[0]), Stream: 77, Burst: [33]byte(header),
	}); len(out) != 1 {
		t.Fatalf("data header: got %d IPSC packets, want 1", len(out))
	}

	got := tr.EndStreams()
	if len(got) != 2 {
		t.Fatalf("got %d packets, want the data end packet and the voice terminator", len(got))
	}
	checkDataEndPacket(t, got[0])
	if hex.EncodeToString(got[1]) != hex.EncodeToString(want) {
		t.Errorf("voice terminator\n got %x\nwant %x", got[1], want)
	}
	if len(tr.streams) != 0 {
		t.Errorf("%d streams left open", len(tr.streams))
	}
	if again := tr.EndStreams(); len(again) != 0 {
		t.Errorf("second EndStreams sent %d packets", len(again))
	}
}
