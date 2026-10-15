package ipsc

import (
	"encoding/hex"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	mmdvm "github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

// rigPICall is a TS2 group call from 3191868 to 9 with a PI header (ALGID
// 0x21, MFID 0x10, key ID 1, MI 0x12345678) and the privacy service option,
// one superframe long, as the XPR 8400 relayed it.
func rigPICall() []string {
	return []string{
		"800004ce2e7130b43c0000090200007113a080dd690838d06ae70000000001c0000a800a006000004000000930b43ce175ec00714970",
		"800004ce2e7130b43c0000090200007113a0805d690938d06cc70000000000c0000a800a006021100112345678000009246e0070494f",
		"800004ce2e7130b43c0000090200007113a0805d690a38d06ea7000000008a1440541cbd8ee6d793cd54372cbe82e8c4004a2a78",
		"800004ce2e7130b43c0000090200007113a0805d690b38d07087000000008a1906dc54392640f6312d4a350e936cf863a3f517500000090072",
		"800004ce2e7130b43c0000090200007113a0805d690c38d07267000000008a19064d5f0fa2997082675922d5678bc4820e4c5618060a470676",
		"800004ce2e7130b43c0000090200007113a0805d690d38d07447000000008a190624461b4658531d9ba79627a12937002adef7b0090c280c76",
		"800004ce2e7130b43c0000090200007113a0805d690e38d07627000000008a2216d01ff05dedbdaeaab3075525e078b0066268c84d27666900004000000930b43c74",
		"800004ce2e7130b43c0000090200007113a0805d690f38d07807000000008a19069fcbcb8ec055b04b8eaa424aa8b8815bfbfc300000000070",
		"800004ce2e7130b43c0000090200007113e0805e691038d079e70000000002c0000a800a006000004000000930b43cee7ae300724957",
	}
}

// rigPICallBursts are the bursts dmrgo encodes for rigPICall, and
// rigPICallKinds their DMRD frame types and data types or voice sequences.
func rigPICallBursts() []string {
	return []string{
		"00954a5e05c80e003f504540dc4dff57d75df5d361841570380037642389b60041",
		"147c924d38a402b9736085831c2dff57d75df5d0cd3f7018e1204ca60f58001603",
		"6f7d2c51611e1813bb0927dee94755fd7df75f71bbfd6724638188a7a161c49575",
		"8a3e8c022d191619eeaa053b03c73000009003849ea5bcfdf812bca894359e323a",
		"4922df4c3e3bd60c0f3432b218977060a4706dd16e595bc6c5fc8e707c1726043d",
		"62b67b404c70f4053a0dca5bf7077090c280cddf8428b241d17b05dd18bf1b0b70",
		"9d3b07147149793a31b2bcd2f0c754d276669ae3805c3f7964072cffc3606f58eb",
		"e7588ce8e83658da7b9e26672e171000000004b0e3074705b103f7dff1760aabbe",
		"00fa4a8a05780e783f2045e0dc8dff57d75df5d4349016483f603b242b89af0072",
	}
}

func rigPICallKinds() [][2]uint {
	return [][2]uint{
		{mmdvmFrameTypeDataSync, uint(enums.DataTypeVoiceLCHeader)},
		{mmdvmFrameTypeDataSync, uint(enums.DataTypePIHeader)},
		{mmdvmFrameTypeVoiceSync, 0},
		{mmdvmFrameTypeVoice, 1}, {mmdvmFrameTypeVoice, 2}, {mmdvmFrameTypeVoice, 3},
		{mmdvmFrameTypeVoice, 4}, {mmdvmFrameTypeVoice, 5},
		{mmdvmFrameTypeDataSync, uint(enums.DataTypeTerminatorWithLC)},
	}
}

func TestRigPICallToMMDVM(t *testing.T) {
	t.Parallel()
	got := translateRig(t, rigPICall())
	checkRigBursts(t, got, rigPICallBursts())
	for i, k := range rigPICallKinds() {
		if got[i].FrameType != k[0] || got[i].DTypeOrVSeq != k[1] {
			t.Errorf("packet %d: frame type %d dtype/vseq %d, want %d %d", i, got[i].FrameType, got[i].DTypeOrVSeq, k[0], k[1])
		}
	}
}

// rigVoiceToIPSC sends bursts with kinds to IPSC as a TS2 group call from
// 3191868 to 9 and returns, for each packet the repeater sent in ref, the
// one the translator built in its place. The three voice headers the
// translator sends stand for the repeater's one.
func rigVoiceToIPSC(t *testing.T, ref, bursts []string, kinds [][2]uint) [][]byte {
	t.Helper()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	tr.SetPeerID(3149999)
	tr.SetColorCode(7)
	got := make([][]byte, 0, len(bursts))
	for i, b := range bursts {
		w, _ := hex.DecodeString(b)
		out := tr.TranslateToIPSC(mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: 9, GroupCall: true, Slot: true,
			FrameType: kinds[i][0], DTypeOrVSeq: kinds[i][1], StreamID: 78, DMRData: [33]byte(w),
		})
		if len(out) == 0 {
			t.Fatalf("burst %d: no packets", i)
		}
		got = append(got, out[len(out)-1])
	}
	if len(got) != len(ref) {
		t.Fatalf("got %d packets, want %d", len(got), len(ref))
	}
	return got
}

// TestRigPICallToIPSC checks the packet type and the burst of each packet
// the translator sends for rigPICall, apart from the RSSI.
func TestRigPICallToIPSC(t *testing.T) {
	t.Parallel()
	ref := rigPICall()
	for i, g := range rigVoiceToIPSC(t, ref, rigPICallBursts(), rigPICallKinds()) {
		r, _ := hex.DecodeString(ref[i])
		n := len(r)
		if r[30] < ipscBurstSlot1 {
			n -= 2
		}
		if g[0] != r[0] || len(g) != len(r) || hex.EncodeToString(g[32:n]) != hex.EncodeToString(r[32:n]) {
			t.Errorf("packet %d:\n got %x\nwant %x", i, g, r)
		}
	}
}

// rigDataCall is a data packet "DMRGO TEST" that the XPR 8400 relayed on
// TS2 from 3191868, the bursts dmrgo encodes for it, and their data types.
type rigDataCall struct {
	name    string
	dst     uint
	group   bool
	packets []string
	bursts  []string
	types   []enums.DataType
}

func rigRate34Calls() []rigDataCall {
	return []rigDataCall{{
		// Unconfirmed, to group 9: the header and the rate 3/4 block, then
		// two 0x26 packets.
		name: "unconfirmed", dst: 9, group: true,
		packets: []string{
			"830004ce2e0430b43c00000901000008972080dd0dc177f60fa00000000006c0000a800a006082a400000930b43c81006a6d00764898",
			"830004ce2e0430b43c000009010000089760805e0dc277f611800000000008c2000d800a009052474f205445535400000000bd3e6a6c444d0078488b",
			"830004ce2e0430b43c000009010000089720805d0dc377f613600000000026c00003800200004883",
			"830004ce2e0430b43c000009010000089720805d0dc477f615400000000026c00003800200004897",
		},
		bursts: []string{
			"081f86c810e222381764d761ddadff57d75df5de683018fa32b8216984480490f3",
			"e669e3f222cdf5d5e00022edde1dff57d75df5d7d4ea2a3a322ce4762f79222166",
		},
		types: []enums.DataType{enums.DataTypeDataHeader, enums.DataTypeRate34},
	}, {
		// Confirmed, to radio 3191868: the header and the rate 3/4 block,
		// then a 0x13 packet.
		name: "confirmed", dst: 3191868,
		packets: []string{
			"840004ce2e8030b43c30b43c0100006ca62080dd6c5461006e330000000006c0000a800a006043a230b43c30b43c8100d06d007646fc",
			"840004ce2e8030b43c30b43c0100006ca660805e6c55610070130000000008c0000d800a0090444d52474f20544553540000a3ecda7c002e007846cf",
			"840004ce2e8030b43c30b43c0100006ca660805e6c56610071f30000000013800000",
		},
		bursts: []string{
			"3849367d31d2a01a1620de61ddadff57d75df5de68245897b13121c880040b24d3",
			"2c3c2a3cf259f24919d542edde1dff57d75df5d7d42efea77a2f9de704bf0f2c4e",
		},
		types: []enums.DataType{enums.DataTypeDataHeader, enums.DataTypeRate34},
	}}
}

func TestRigRate34ToMMDVM(t *testing.T) {
	t.Parallel()
	for _, c := range rigRate34Calls() {
		got := translateRig(t, c.packets)
		if len(got) != len(c.bursts) {
			t.Fatalf("%s: got %d packets, want %d", c.name, len(got), len(c.bursts))
		}
		for i, p := range got {
			w, _ := hex.DecodeString(c.bursts[i])
			if p.DMRData != [33]byte(w) {
				t.Errorf("%s packet %d: burst\n got %x\nwant %x", c.name, i, p.DMRData, w)
			}
			if p.Src != 3191868 || p.Dst != c.dst || p.GroupCall != c.group || !p.Slot ||
				p.FrameType != mmdvmFrameTypeDataSync || p.DTypeOrVSeq != uint(c.types[i]) {
				t.Errorf("%s packet %d: got %+v", c.name, i, p)
			}
		}
	}
}

// rigDataToIPSC sends the bursts of c to IPSC and checks bytes 30 to the
// RSSI of each packet against what the XPR 8400 sent. Bit 1 of byte 31,
// which the repeater sets when a block fails its CRC check, is not compared:
// the repeater sets it on every unconfirmed rate 3/4 block, which has no
// CRC-9.
func rigDataToIPSC(t *testing.T, c rigDataCall) [][]byte {
	t.Helper()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	tr.SetPeerID(3149999)
	tr.SetColorCode(7)
	all := make([][]byte, 0, len(c.bursts))
	for i, b := range c.bursts {
		w, _ := hex.DecodeString(b)
		pkt := mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: c.dst, GroupCall: c.group, Slot: true,
			FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(c.types[i]), StreamID: 77, DMRData: [33]byte(w),
		}
		out := tr.TranslateToIPSC(pkt)
		if len(out) != 1 {
			t.Fatalf("%s burst %d: got %d packets", c.name, i, len(out))
		}
		r, _ := hex.DecodeString(c.packets[i])
		if len(out[0]) != len(r) {
			t.Fatalf("%s burst %d: got %d bytes, want %d", c.name, i, len(out[0]), len(r))
		}
		n := len(r) - 2
		g := append([]byte(nil), out[0][30:n]...)
		g[1] &^= 0x02
		r[31] &^= 0x02
		if hex.EncodeToString(g) != hex.EncodeToString(r[30:n]) {
			t.Errorf("%s burst %d: bytes 30-%d\n got %x\nwant %x", c.name, i, n-1, g, r[30:n])
		}
		all = append(all, out[0])
	}
	return all
}

func TestRigRate34ToIPSC(t *testing.T) {
	t.Parallel()
	for _, c := range rigRate34Calls() {
		rigDataToIPSC(t, c)
	}
}

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

// TestRigLostBurstAAfterHeader uses a call where the XPR 8400 missed burst A
// after the voice header: it sent two headers, then bursts B to F with no
// gap in the RTP sequence, a terminator, and two 0x26 packets.
func TestRigLostBurstAAfterHeader(t *testing.T) {
	t.Parallel()
	got := translateRig(t, []string{
		"800004ce2e3230b43c00000902000058f320805dfea03bc583a40000000001c0000a800a006000000000000930b43ce9f8ed00714751",
		"800004ce2e3230b43c00000902000058f320805dfea13bc585840000000001c0000a800a006000000000000930b43ce9f8ed00714751",
		"800004ce2e3230b43c00000902000058f320805dfea23bc58764000000008a1906dc54392640f6312d4a350e936cf863a3f517500000090072",
		"800004ce2e3230b43c00000902000058f320805dfea33bc58944000000008a19064d5f0fa2997082675922d5678bc4820e4c5618060a060676",
		"800004ce2e3230b43c00000902000058f320805dfea43bc58b24000000008a190624461b4658531d9ba79627a12937002adef7b0090c240c76",
		"800004ce2e3230b43c00000902000058f320805dfea53bc58d04000000008a2216d01ff05dedbdaeaab3075525e078b0066268c80c2b2b2400000000000930b43c74",
		"800004ce2e3230b43c00000902000058f320805dfea63bc58ee4000000008a19069fcbcb8ec055b04b8eaa424aa8b8815bfbfc300000000070",
		"800004ce2e3230b43c00000902000058f360805efea73bc590c40000000002c0000a800a006000000000000930b43ce6f7e20072470e",
		"800004ce2e3230b43c00000902000058f320805dfea83bc592a40000000026c000038002000046f9",
		"800004ce2e3230b43c00000902000058f320805dfea93bc594840000000026c0000380020000472d",
	})
	all := rigVoiceCallBursts()
	want := append([]string{all[0]}, all[2:]...)
	checkRigBursts(t, got, want)
	for i, p := range got[1:6] {
		if p.FrameType != mmdvmFrameTypeVoice || p.DTypeOrVSeq != uint(burstB+i) {
			t.Errorf("voice packet %d: frame type %d vseq %d, want voice burst %c", i, p.FrameType, p.DTypeOrVSeq, 'B'+i)
		}
	}
}

// TestRigLateEntryHeader uses a voice header the XPR 8400 built from the
// embedded LC of a TS2 group call from 3191868 to 91 that it joined late:
// the LC octets with zero RS parity. The header sent to MMDVM must carry
// that LC with valid parity, the same burst dmrgo encodes for the call.
func TestRigLateEntryHeader(t *testing.T) {
	t.Parallel()
	got := translateRig(t, []string{
		"800004ce2e4130b43c00005b0200007f1d2080dd1b0e75c81ced00000000018000081000006000000000005b30b43c000000",
	})
	want, err := hex.DecodeString("00790b8215400f70752005605c4dff57d75df5d361001678387037202f019f8273")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DMRData != [33]byte(want) {
		t.Fatalf("got %d packets %+v, want one with burst %x", len(got), got, want)
	}
}

func TestLateEntryLCRejectsMismatch(t *testing.T) {
	t.Parallel()
	payload := [12]byte{0, 0, 0, 0, 0, 91, 0x30, 0xb4, 0x3c}
	if _, ok := lateEntryLC(payload, 3191868, 91, true); !ok {
		t.Fatal("matching LC rejected")
	}
	if _, ok := lateEntryLC(payload, 3191868, 92, true); ok {
		t.Fatal("LC for another destination accepted")
	}
	if _, ok := lateEntryLC(payload, 3191868, 91, false); ok {
		t.Fatal("group LC accepted for a private call")
	}
	payload[11] = 1
	if _, ok := lateEntryLC(payload, 3191868, 91, true); ok {
		t.Fatal("LC with nonzero parity accepted")
	}
}
