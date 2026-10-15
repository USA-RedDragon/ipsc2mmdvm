package ipsc

import (
	"encoding/hex"
	"testing"

	mmdvm "github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

// Packets an XPR 8400 (peer 314926, color code 7) sent on IPSC for calls
// that dmrgo's dmrrf transmitted into it over RF on 2026-10-15, and the DMR
// bursts dmrgo encodes for the same content with BS-sourced SYNC. The bursts
// that the translator builds from the packets must be the same bytes.

// rigVoiceCall is a TS2 group call from 3191868 to 9: the voice LC header,
// one superframe of bursts A to F with dmrrf's AMBE test pattern, and the
// terminator.
func rigVoiceCall() []string {
	return []string{
		"800004ce2e3230b43c00000902000058f320805dfb283bbf02a40000000001c0000a800a006000000000000930b43ce9f8ed00714694",
		"800004ce2e3230b43c00000902000058f320805dfb293bbf0484000000008a1440541cbd8ee6d793cd54372cbe82e8c4004a2a78",
		"800004ce2e3230b43c00000902000058f320805dfb2a3bbf0664000000008a1906dc54392640f6312d4a350e936cf863a3f517500000090072",
		"800004ce2e3230b43c00000902000058f320805dfb2b3bbf0844000000008a19064d5f0fa2997082675922d5678bc4820e4c5618060a060676",
		"800004ce2e3230b43c00000902000058f320805dfb2c3bbf0a24000000008a190624461b4658531d9ba79627a12937002adef7b0090c240c76",
		"800004ce2e3230b43c00000902000058f320805dfb2d3bbf0c04000000008a2216d01ff05dedbdaeaab3075525e078b0066268c80c2b2b2400000000000930b43c74",
		"800004ce2e3230b43c00000902000058f320805dfb2e3bbf0de4000000008a19069fcbcb8ec055b04b8eaa424aa8b8815bfbfc300000000070",
		"800004ce2e3230b43c00000902000058f360805efb2f3bbf0fc40000000002c0000a800a006000000000000930b43ce6f7e20072467f",
	}
}

// rigVoiceCallBursts are the bursts of rigVoiceCall.
func rigVoiceCallBursts() []string {
	return []string{
		"00d40ada05cc0e703f004d80dc4dff57d75df5d36194140039c03be02f01b20054",
		"6f7d2c51611e1813bb0927dee94755fd7df75f71bbfd6724638188a7a161c49575",
		"8a3e8c022d191619eeaa053b03c73000009003849ea5bcfdf812bca894359e323a",
		"4922df4c3e3bd60c0f3432b218977060a0606dd16e595bc6c5fc8e707c1726043d",
		"62b67b404c70f4053a0dca5bf7077090c240cddf8428b241d17b05dd18bf1b0b70",
		"9d3b07147149793a31b2bcd2f0c750c2b2b24ae3805c3f7964072cffc3606f58eb",
		"e7588ce8e83658da7b9e26672e171000000004b0e3074705b103f7dff1760aabbe",
		"00bb0a0e057c0e083f704d20dc8dff57d75df5d4348017383ea037a02701ab0067",
	}
}

// translateRig feeds hex IPSC packets to a translator with color code 7 and
// returns the MMDVM packets it builds.
func translateRig(t *testing.T, packets []string) []mmdvm.Packet {
	t.Helper()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	tr.SetPeerID(3149999)
	tr.SetColorCode(7)
	out := make([]mmdvm.Packet, 0, len(packets))
	for _, h := range packets {
		data, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tr.TranslateToMMDVM(data[0], data)...)
	}
	return out
}

// checkRigBursts checks that got carries exactly the want bursts in order,
// on TS2 for the group call 3191868 to 9.
func checkRigBursts(t *testing.T, got []mmdvm.Packet, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d packets, want %d", len(got), len(want))
	}
	for i, p := range got {
		w, err := hex.DecodeString(want[i])
		if err != nil {
			t.Fatal(err)
		}
		if [33]byte(w) != p.DMRData {
			t.Errorf("packet %d (frame type %d, dtype/vseq %d): burst\n got %x\nwant %x", i, p.FrameType, p.DTypeOrVSeq, p.DMRData, w)
		}
		if p.Src != 3191868 || p.Dst != 9 || !p.GroupCall || !p.Slot {
			t.Errorf("packet %d: got src %d dst %d group %v ts2 %v", i, p.Src, p.Dst, p.GroupCall, p.Slot)
		}
	}
}

func TestRigVoiceCallBursts(t *testing.T) {
	t.Parallel()
	checkRigBursts(t, translateRig(t, rigVoiceCall()), rigVoiceCallBursts())
}
