package ipsc

import (
	"encoding/hex"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/packet"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/vocoder"
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
		rigBurstA,
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

// TestRigPICallToIPSC checks the packet type, the call info with the
// privacy flag, and the burst of each packet the translator sends for
// rigPICall, apart from the RSSI.
func TestRigPICallToIPSC(t *testing.T) {
	t.Parallel()
	ref := rigPICall()
	for i, g := range rigVoiceToIPSC(t, ref, rigPICallBursts(), rigPICallKinds()) {
		r, _ := hex.DecodeString(ref[i])
		n := len(r)
		if r[30] < ipscBurstSlot1 {
			n -= 2
		}
		if g[0] != r[0] || g[17] != r[17] || len(g) != len(r) || hex.EncodeToString(g[32:n]) != hex.EncodeToString(r[32:n]) {
			t.Errorf("packet %d:\n got %x\nwant %x", i, g, r)
		}
	}
}

// TestPIHeaderSetsPrivacy sends the voice header of rigVoiceCall, whose LC
// has no privacy service option, then the PI header and burst A of
// rigPICall. The PI header alone makes the call encrypted, so it and every
// packet after it carry the privacy flag.
func TestPIHeaderSetsPrivacy(t *testing.T) {
	t.Parallel()
	bursts := []string{rigVoiceCallBursts()[0], rigPICallBursts()[1], rigPICallBursts()[2]}
	kinds := rigPICallKinds()[:3]
	got := rigVoiceToIPSC(t, bursts, bursts, kinds)
	for i, g := range got {
		if want := i > 0; (g[17]&ipscCallInfoPrivacy != 0) != want {
			t.Errorf("packet %d: call info %02x, want privacy %v", i, g[17], want)
		}
	}
}

// rigBurstA is burst A of the first superframe of dmrrf's AMBE test
// pattern, BS-sourced, color code 7. Burst A carries no LC, so it is the
// same in every test call.
const rigBurstA = "6f7d2c51611e1813bb0927dee94755fd7df75f71bbfd6724638188a7a161c49575"

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

// rigDataToIPSC sends the bursts of c to IPSC and checks the packet type,
// the call priority, the call info with the end flag on the last packet,
// the RTP marker and payload type, and bytes 30 to the RSSI of each packet
// against what the XPR 8400 sent. Bit 1 of byte 31,
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
		if out[0][0] != r[0] || out[0][12] != r[12] || out[0][17] != r[17] || out[0][19] != r[19] {
			t.Errorf("%s burst %d: packet type %02x priority %02x call info %02x RTP %02x, want %02x %02x %02x %02x",
				c.name, i, out[0][0], out[0][12], out[0][17], out[0][19], r[0], r[12], r[17], r[19])
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

// checkDataEndPacket checks that p is the packet that ends a data call
// when its last block is dropped, in the form an XPR 8400 sends it on
// losing the signal: the 0x13 packet the repeater sent after the confirmed
// rate 3/4 packet of rigRate34Calls, apart from the peer, byte 5, the call
// control and RTP sequence and timestamp.
func checkDataEndPacket(t *testing.T, p []byte) {
	t.Helper()
	r, _ := hex.DecodeString(rigRate34Calls()[1].packets[2])
	if len(p) != len(r) || p[0] != r[0] || p[12] != r[12] || p[17] != r[17] ||
		hex.EncodeToString(p[18:20]) != hex.EncodeToString(r[18:20]) ||
		hex.EncodeToString(p[26:]) != hex.EncodeToString(r[26:]) {
		t.Errorf("end packet\n got %x\nwant %x", p, r)
	}
}

// TestUndecodableLastBlockEndsCall sends the confirmed rate 3/4 packet of
// rigRate34Calls with its block damaged beyond trellis correction. The
// block is dropped, but it is still the last block of the packet, so the
// call ends with an end packet in its place.
func TestUndecodableLastBlockEndsCall(t *testing.T) {
	t.Parallel()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	tr.SetColorCode(7)
	c := rigRate34Calls()[1]
	for i, h := range c.bursts {
		w, _ := hex.DecodeString(h)
		if i == 1 {
			for j := 0; j < 13; j++ {
				w[j] ^= 0xA5
			}
		}
		out := tr.TranslateToIPSC(mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: c.dst, Slot: true,
			FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(c.types[i]), StreamID: 80, DMRData: [33]byte(w),
		})
		if len(out) != 1 {
			t.Fatalf("burst %d: got %d packets, want 1", i, len(out))
		}
		if i == 1 {
			checkDataEndPacket(t, out[0])
		}
	}
	if len(tr.streams) != 0 {
		t.Fatalf("call still open after its last block: %d streams", len(tr.streams))
	}
}

// TestRate1LastBlockEndsCall sends a confirmed rate 1 packet from 3191868
// to 3140598 on TS2, as an MMDVM master sent it on the rig. IPSC cannot
// carry the rate 1 block, so it is dropped, and an end packet ends the call
// in its place.
func TestRate1LastBlockEndsCall(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	bursts := []string{
		"202d367370eee0d191d3dd2fddadff57d75df5de68e8f94673e020ae89c2082cfb",
		"0050444d52474f20544553541ebdff57d75df5d32c0000000000000000d1d4302d",
	}
	types := []enums.DataType{enums.DataTypeDataHeader, enums.DataTypeRate1}
	got := make([][]byte, 0, len(bursts))
	for i, h := range bursts {
		w, _ := hex.DecodeString(h)
		got = append(got, tr.TranslateToIPSC(mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: 3140598, Slot: true,
			FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(types[i]), StreamID: 84, DMRData: [33]byte(w),
		})...)
	}
	if len(got) != 2 || got[0][30] != ipscBurstDataHeader {
		t.Fatalf("got %x, want the data header and an end packet", got)
	}
	checkDataEndPacket(t, got[1])
	if string(got[1][13:17]) != string(got[0][13:17]) || got[1][5] != got[0][5] {
		t.Errorf("end packet is not in the call of the header:\n%x\n%x", got[0], got[1])
	}
	if len(tr.streams) != 0 {
		t.Fatalf("call still open after its last block: %d streams", len(tr.streams))
	}
}

func TestRigRate34ToIPSC(t *testing.T) {
	t.Parallel()
	for _, c := range rigRate34Calls() {
		rigDataToIPSC(t, c)
	}
}

// TestRate34CallEndsAtLastBlock sends the unconfirmed rate 3/4 packet of
// rigRate34Calls twice with one stream ID, as an MMDVM master may send a
// packet and its repeat. The last block ends the first call, so the second
// is a new call with a new call control and the RTP marker.
func TestRate34CallEndsAtLastBlock(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	c := rigRate34Calls()[0]
	first := make([][]byte, 0, 2*len(c.bursts))
	for range 2 {
		for i, h := range c.bursts {
			w, _ := hex.DecodeString(h)
			out := tr.TranslateToIPSC(mmdvm.Packet{
				Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: c.dst, GroupCall: true, Slot: true,
				FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(c.types[i]), StreamID: 81, DMRData: [33]byte(w),
			})
			if len(out) != 1 {
				t.Fatalf("burst %d: got %d packets", i, len(out))
			}
			first = append(first, out[0])
		}
		if len(tr.streams) != 0 {
			t.Fatalf("call still open after its last block: %d streams", len(tr.streams))
		}
	}
	if string(first[0][13:17]) == string(first[2][13:17]) || first[2][19] != 0xDD {
		t.Errorf("repeat is not a new call:\n first %x\nrepeat %x", first[0], first[2])
	}
}

// TestRate34MessageCRCFailureEndsCall sends the unconfirmed rate 3/4 packet
// of rigRate34Calls with a user data octet of its block changed, so the
// block decodes but the message CRC fails. The packet is still complete,
// so its last block carries the end flag.
func TestRate34MessageCRCFailureEndsCall(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	c := rigRate34Calls()[0]
	for i, h := range c.bursts {
		w, _ := hex.DecodeString(h)
		if i == 1 {
			block, res := layer2.RawBurst(w).Rate34Payload()
			if res.Uncorrectable {
				t.Fatal("block does not decode")
			}
			block[3] ^= 0x01
			b, err := layer2.BuildRate34DataBurst(block, 7, layer2.SyncSourceBS)
			if err != nil {
				t.Fatal(err)
			}
			w = b[:]
		}
		out := tr.TranslateToIPSC(mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: c.dst, GroupCall: true, Slot: true,
			FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(c.types[i]), StreamID: 82, DMRData: [33]byte(w),
		})
		if len(out) != 1 {
			t.Fatalf("burst %d: got %d packets", i, len(out))
		}
		if end := out[0][17]&ipscCallInfoEnd != 0; end != (i == 1) {
			t.Errorf("burst %d: end flag %v", i, end)
		}
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
		rigBurstA,
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

// TestRigBadFramesAreSilence uses a late-entry call to group 91 where the
// XPR 8400 flagged the first AMBE frame of burst E as bad (byte 32 0x17).
// That frame differs from the frame dmrrf sent (0x152ee72f3454f) in 11
// bits, so it goes to MMDVM as silence. The test also flags the second frame
// of burst B and the third frame of burst C in bits 49 and 99 of their AMBE
// payloads, which go to MMDVM as silence too. The other frames pass as they
// are.
func TestRigBadFramesAreSilence(t *testing.T) {
	t.Parallel()
	ipsc := []string{
		"800004ce2e4130b43c00005b0200007f1d2080dd1b0e75c81ced00000000018000081000006000000000005b30b43c000000",
		"800004ce2e4130b43c00005b0200007f1d20805d1b0f75c81ecd00000000018000081000006000000000005b30b43c000000",
		"800004ce2e4130b43c00005b0200007f1d20805d1b1075c820ad000000008a144074d041c867b211a56b4890b58b0158805cf6b8",
		"800004ce2e4130b43c00005b0200007f1d20805d1b1175c8228d000000008a1906c5490c12f886be3b21b945ed4b56b3de931a600009090072",
		"800004ce2e4130b43c00005b0200007f1d20805d1b1275c8246d000000008a190600ea7aa07934905e3b6989ef6175e4911998000f0a060676",
		"800004ce2e4130b43c00005b0200007f1d20805d1b1375c8264d000000008a19064f5abef16a8813b5768b8c62c0ebd07a69c390091d0f0576",
		"800004ce2e4130b43c00005b0200007f1d20805d1b1475c8282d000000008a221779f72e7862a7a6bbd51c8044af40324b1dd5801d18001e00000000005b30b43c74",
		"800004ce2e4130b43c00005b0200007f1d20805d1b1575c82a0d000000008a1906169b888cc2948e83eb33b8dee87bac9e8846180000000070",
		"800004ce2e4130b43c00005b0200007f1d60805e1b1675c82bed0000000002c0000a800a006000000000005b30b43c4c735c007246f9",
	}
	flagged := map[int][3]bool{burstB: {false, true, false}, burstC: {false, false, true}}
	for pos, bad := range flagged {
		p, _ := hex.DecodeString(ipsc[pos+2])
		frames := vocoder.UnpackAMBEVoice([19]byte(p[33:52]))
		ambe := vocoder.PackAMBEVoiceFlags(frames, bad)
		copy(p[33:52], ambe[:])
		ipsc[pos+2] = hex.EncodeToString(p)
	}
	got := translateRig(t, ipsc)
	if len(got) != 8 {
		t.Fatalf("got %d packets, want 8", len(got))
	}
	for i, h := range ipsc[2:8] {
		p := got[i+1]
		var b layer2.Burst
		_ = b.DecodeFromBytesWith(p.DMRData, mmdvmVoiceDecodeOptions(p))
		frames, _ := b.AMBEFrames()
		in, _ := hex.DecodeString(h)
		want := vocoder.UnpackAMBEVoice([19]byte(in[33:52]))
		if i == burstE {
			want[0] = vocoder.AMBESilence()
		}
		for f, bad := range flagged[i] {
			if bad {
				want[f] = vocoder.AMBESilence()
			}
		}
		if frames != want {
			t.Errorf("burst %c: frames %v, want %v", 'A'+i, frames, want)
		}
	}
}

// TestRigSACKResponseToMMDVM uses a selective retry response from 3140598
// to 3191868 on TS2 that the XPR 8400 relayed: a response header and one
// C_RDATA block asking again for blocks 1, 4 and 6 of an 8-block packet.
// The bursts sent to MMDVM must decode to the same response with dmrgo.
func TestRigSACKResponseToMMDVM(t *testing.T) {
	t.Parallel()
	got := translateRig(t, []string{
		"840004ce2e742febf630b43c0100006bc22080ddd7133445d8f20000000006c0000a800a006001a030b43c2febf681800a520076496f",
		"840004ce2e742febf630b43c0100006bc260805ed7143445dad20000000007c0000a800a0060b5ffffffffffffff0f341fd70077497f",
	})
	if len(got) != 2 {
		t.Fatalf("got %d packets, want 2", len(got))
	}
	hb, err := layer2.NewBurstFromBytes(got[0].DMRData)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := hb.Data.(*pdu.DataHeader)
	if !ok || h.ResponsePacketHeader == nil || hb.FEC.PDU.Uncorrectable {
		t.Fatalf("header burst decodes to %T %+v, fec %+v", hb.Data, hb.Data, hb.FEC)
	}
	r := h.ResponsePacketHeader
	if r.LLIDSource != 3140598 || r.LLIDDestination != 3191868 || r.ResponseClass != 2 || r.BlocksToFollow != 1 {
		t.Errorf("response header %+v", r)
	}
	db, err := layer2.NewBurstFromBytes(got[1].DMRData)
	if err != nil {
		t.Fatal(err)
	}
	block, ok := db.Data.(*pdu.Rate12Data)
	if !ok {
		t.Fatalf("C_RDATA burst decodes to %T", db.Data)
	}
	rd, err := packet.DecodeResponseData(block)
	if err != nil {
		t.Fatal(err)
	}
	if retry := rd.RetryBlocks(); len(retry) != 3 || retry[0] != 1 || retry[1] != 4 || retry[2] != 6 {
		t.Errorf("retry blocks %v, want [1 4 6]", retry)
	}
	for i, p := range got {
		if p.Src != 3140598 || p.Dst != 3191868 || p.GroupCall || !p.Slot || p.FrameType != mmdvmFrameTypeDataSync {
			t.Errorf("packet %d: %+v", i, p)
		}
	}
}

// TestRigRate1BlockIsDropped uses a confirmed rate 1 packet from 3191868 to
// 3140598 on TS1. The XPR 8400 relayed its data header, then the rate 1
// block as a 0x06 packet with a meaningless BPTC(196,96) decode that fails
// the data header CRC (byte 31 bit 1 set), then a 0x13 end packet. Only the
// header goes to MMDVM.
func TestRigRate1BlockIsDropped(t *testing.T) {
	t.Parallel()
	got := translateRig(t, []string{
		"840004ce2e6830b43c2febf6010000251c00805d2dd20f396f4f000000000640000a800a006043a82febf630b43c810083cf00765eba",
		"840004ce2e6830b43c2febf6010000251c40805e2dd30f39712f000000000642000a800a006013054441204104844141601500765e7a",
		"840004ce2e6830b43c2febf6010000251c40805e2dd40f39730f0000000013000000",
	})
	if len(got) != 1 || got[0].DTypeOrVSeq != uint(enums.DataTypeDataHeader) {
		t.Fatalf("got %+v, want the data header only", got)
	}
}

// TestMBCContinuationPassesWithoutCRC sends a TS1 MBC continuation packet
// in the form an XPR 8400 sends, taken from a capture of uncertain origin.
// Only the last block of an MBC carries a CRC, so the block goes to MMDVM
// with its payload although it fails a CSBK CRC check.
func TestMBCContinuationPassesWithoutCRC(t *testing.T) {
	t.Parallel()
	ipsc := "840004ce2e1230b43c30b43c01000063c600805d958f41379d0b000000000540000a800a0060f07dadbd2aaff6880c9c22120025467c"
	d, _ := hex.DecodeString(ipsc)
	if layer2.MMDVMHostAccepts(enums.DataTypeMBCContinuation, [12]byte(d[38:50])) {
		t.Fatal("payload passes the CRC check")
	}
	got := translateRig(t, []string{ipsc})
	if len(got) != 1 || got[0].DTypeOrVSeq != uint(enums.DataTypeMBCContinuation) {
		t.Fatalf("got %+v, want the MBC continuation", got)
	}
	if p, _ := layer2.RawBurst(got[0].DMRData).BPTCPayload(); p != [12]byte(d[38:50]) {
		t.Errorf("payload %x, want %x", p, d[38:50])
	}
}

// rigLateEntryBursts are bursts A to E of the first superframe of a TS2
// group call from 3191868 to 91 that starts without a voice header.
func rigLateEntryBursts() []string {
	return []string{
		rigBurstA,
		"8a3e8c022d191619eeaa053b03c73000909003849ea5bcfdf812bca894359e323a",
		"4922df4c3e3bd60c0f3432b2189770f0a0606dd16e595bc6c5fc8e707c1726043d",
		"62b67b404c70f4053a0dca5bf7077091d0f05ddf8428b241d17b05dd18bf1b0b70",
		"9d3b07147149793a31b2bcd2f0c751d18001eae3805c3f7964072cffc3606f58eb",
	}
}

// TestRigLateEntryToIPSCCarriesEmbeddedLC sends the first superframe of a
// TS2 group call from 3191868 to 91 that starts without a voice header. The
// XPR 8400 builds the voice header of such a call from the LC in bytes 56-64
// of burst E, so burst E must carry the embedded LC of the call, as the
// repeater itself sent it for the same call ("1d18001e", the LC, EMB 0x74),
// not one built with other service options.
func TestRigLateEntryToIPSCCarriesEmbeddedLC(t *testing.T) {
	t.Parallel()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	tr.SetPeerID(3149999)
	tr.SetColorCode(7)
	bursts := rigLateEntryBursts()
	var e []byte
	for i, h := range bursts {
		w, _ := hex.DecodeString(h)
		ft := mmdvmFrameTypeVoice
		if i == burstA {
			ft = mmdvmFrameTypeVoiceSync
		}
		out := tr.TranslateToIPSC(mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: 91, GroupCall: true, Slot: true,
			FrameType: ft, DTypeOrVSeq: uint(i), StreamID: 79, DMRData: [33]byte(w),
		})
		if len(out) != 1 {
			t.Fatalf("burst %d: got %d packets", i, len(out))
		}
		e = out[0]
	}
	if got, want := hex.EncodeToString(e[52:]), "1d18001e00000000005b30b43c74"; got != want {
		t.Fatalf("burst E bytes 52-65: got %s, want %s", got, want)
	}
}

// lateEntryToIPSC sends bursts, hex-encoded MMDVM voice bursts of one
// superframe from burst A on, to IPSC as a TS2 call from 3191868 to dst
// with no voice header, and returns the packet built for each.
func lateEntryToIPSC(t *testing.T, bursts []string, dst uint, group bool) [][]byte {
	t.Helper()
	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	got := make([][]byte, 0, len(bursts))
	for i, h := range bursts {
		w, _ := hex.DecodeString(h)
		ft := mmdvmFrameTypeVoice
		if i == burstA {
			ft = mmdvmFrameTypeVoiceSync
		}
		out := tr.TranslateToIPSC(mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: dst, GroupCall: group, Slot: true,
			FrameType: ft, DTypeOrVSeq: uint(i), StreamID: 83, DMRData: [33]byte(w),
		})
		if len(out) != 1 {
			t.Fatalf("burst %d: got %d packets", i, len(out))
		}
		got = append(got, out[0])
	}
	return got
}

// TestLateEntryIgnoresLCOfOtherCallType sends rigLateEntryBursts, whose
// embedded LC is a group voice LC, as a private call. The LC does not match
// the call, so burst E carries the unit-to-unit LC built from the packet
// fields.
func TestLateEntryIgnoresLCOfOtherCallType(t *testing.T) {
	t.Parallel()
	got := lateEntryToIPSC(t, rigLateEntryBursts(), 91, false)
	if lc, want := hex.EncodeToString(got[burstE][56:65]), "03002000005b30b43c"; lc != want {
		t.Fatalf("burst E LC %s, want %s", lc, want)
	}
}

// TestLateEntryLearnsPrivacy joins rigPICall at burst A. Its embedded LC
// has the privacy service option, so the call is encrypted from the burst
// after E, which completes the LC.
func TestLateEntryLearnsPrivacy(t *testing.T) {
	t.Parallel()
	got := lateEntryToIPSC(t, rigPICallBursts()[2:8], 9, true)
	for i, p := range got {
		if want := i == burstF; (p[17]&ipscCallInfoPrivacy != 0) != want {
			t.Errorf("burst %d: call info %02x, want privacy %v", i, p[17], want)
		}
	}
}

// TestLateEntryNeverMixesSuperframes sends the bursts of
// TestRigLateEntryToIPSCCarriesEmbeddedLC with E lost in the first
// superframe and A to D lost in the second. Fragments B-D of the first and
// E of the second decode to the call's LC, but they are from two
// superframes, so burst E carries the LC built from the packet fields.
func TestLateEntryNeverMixesSuperframes(t *testing.T) {
	t.Parallel()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	tr.SetPeerID(3149999)
	tr.SetColorCode(7)
	bursts := []string{
		rigBurstA,
		"8a3e8c022d191619eeaa053b03c73000909003849ea5bcfdf812bca894359e323a",
		"4922df4c3e3bd60c0f3432b2189770f0a0606dd16e595bc6c5fc8e707c1726043d",
		"62b67b404c70f4053a0dca5bf7077091d0f05ddf8428b241d17b05dd18bf1b0b70",
		"9d3b07147149793a31b2bcd2f0c751d18001eae3805c3f7964072cffc3606f58eb",
	}
	var e []byte
	for _, pos := range []int{burstA, burstB, burstC, burstD, burstF, burstE} {
		b := pos
		if pos == burstF {
			b = burstB
		}
		w, _ := hex.DecodeString(bursts[b])
		ft := mmdvmFrameTypeVoice
		if pos == burstA {
			ft = mmdvmFrameTypeVoiceSync
		}
		out := tr.TranslateToIPSC(mmdvm.Packet{
			Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: 91, GroupCall: true, Slot: true,
			FrameType: ft, DTypeOrVSeq: uint(pos), StreamID: 81, DMRData: [33]byte(w),
		})
		if len(out) != 1 {
			t.Fatalf("burst %c: got %d packets", 'A'+pos, len(out))
		}
		e = out[0]
	}
	want := constructLC(3191868, 91, true)
	if got := hex.EncodeToString(e[56:65]); got != hex.EncodeToString(want[:]) {
		t.Fatalf("burst E LC %s, want %x", got, want)
	}
}

// TestRigCallSequence checks that byte 5 is the same in every packet of a
// call, voice or data, as in the XPR 8400 captures, and differs between
// calls.
func TestRigCallSequence(t *testing.T) {
	t.Parallel()
	for _, ref := range [][]string{rigPICall(), rigRate34Calls()[0].packets, rigRate34Calls()[1].packets} {
		first, _ := hex.DecodeString(ref[0])
		for _, h := range ref {
			r, _ := hex.DecodeString(h)
			if r[5] != first[5] {
				t.Fatalf("capture: byte 5 %02x then %02x", first[5], r[5])
			}
		}
	}
	voice := rigVoiceToIPSC(t, rigPICall(), rigPICallBursts(), rigPICallKinds())
	for i, g := range voice {
		if g[5] != voice[0][5] {
			t.Errorf("voice packet %d: byte 5 %02x, want %02x", i, g[5], voice[0][5])
		}
	}
	data := rigDataToIPSC(t, rigRate34Calls()[1])
	for i, g := range data {
		if g[5] != data[0][5] {
			t.Errorf("data packet %d: byte 5 %02x, want %02x", i, g[5], data[0][5])
		}
	}

	tr := newTestTranslator(t)
	a := tr.TranslateToIPSC(makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, uint(enums.DataTypeCSBK)))
	b := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, uint(enums.DataTypeCSBK))
	b.StreamID++
	if out := tr.TranslateToIPSC(b); len(a) != 1 || len(out) != 1 || a[0][5] == out[0][5] {
		t.Errorf("two calls share byte 5")
	}
}

// rigConfirmedRate12Packet returns the bursts and data types of a confirmed
// rate 1/2 packet from 3191868 to 3140598 on TS1, as an MMDVM master sent
// it on the rig, with the header CRC broken when badCRC is set.
func rigConfirmedRate12Packet(badCRC bool) ([][33]byte, []enums.DataType) {
	bursts := make([][33]byte, 0, 3)
	for i, h := range []string{
		"20f7370570bee26393b7cda7ddadff57d75df5de6accf9067160278e89420a3c95",
		"06dd55694b3555ba4425c5649dcdff57d75df5ddc51065d0864171a60d174dab0b",
		"403201b0010f0182003002c01dcdff57d75df5ddc42001b002541b4c08007630d5",
	} {
		w, _ := hex.DecodeString(h)
		raw := layer2.RawBurst(w)
		if i == 0 && badCRC {
			p, _ := raw.BPTCPayload()
			p[11] ^= 0x01
			raw.SetBPTCPayload(p)
		}
		bursts = append(bursts, raw)
	}
	return bursts, []enums.DataType{enums.DataTypeDataHeader, enums.DataTypeRate12, enums.DataTypeRate12}
}

// TestDroppedHeaderDropsItsBlocks sends rigConfirmedRate12Packet with and
// without a bit of the header CRC flipped. A header that fails its CRC is
// dropped, and dmrgo's packet assembler drops it too, as MMDVMHost does, so
// its blocks have no data packet to belong to. MMDVMHost discards such
// blocks, and so does the translator: no call is left open without an end.
// The intact packet goes through and ends at its last block. A rate 3/4
// block of rigRate34Calls with no header before it is dropped the same way.
func TestDroppedHeaderDropsItsBlocks(t *testing.T) {
	t.Parallel()
	for _, badCRC := range []bool{true, false} {
		tr := newTestTranslator(t)
		bursts, types := rigConfirmedRate12Packet(badCRC)
		for i, b := range bursts {
			out := tr.TranslateToIPSC(mmdvm.Packet{
				Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: 3140598,
				FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(types[i]), StreamID: 85, DMRData: b,
			})
			want := 1
			if badCRC {
				want = 0
			}
			if len(out) != want {
				t.Fatalf("bad CRC %v burst %d: got %d packets, want %d", badCRC, i, len(out), want)
			}
			if want == 1 && (out[0][17]&ipscCallInfoEnd != 0) != (i == 2) {
				t.Errorf("bad CRC %v burst %d: call info %02x", badCRC, i, out[0][17])
			}
		}
		if len(tr.streams) != 0 {
			t.Fatalf("bad CRC %v: call still open after its last block: %d streams", badCRC, len(tr.streams))
		}
	}
	c := rigRate34Calls()[0]
	block, _ := hex.DecodeString(c.bursts[1])
	tr := newTestTranslator(t)
	if out := tr.TranslateToIPSC(mmdvm.Packet{
		Signature: mmdvm.DMRDSignature, Src: 3191868, Dst: c.dst, GroupCall: c.group, Slot: true,
		FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(c.types[1]), StreamID: 86, DMRData: [33]byte(block),
	}); len(out) != 0 || len(tr.streams) != 0 {
		t.Errorf("rate 3/4 block with no header: got %d packets and %d streams, want none", len(out), len(tr.streams))
	}
}

// TestDataCallInProgress follows rigConfirmedRate12Packet: the intact packet
// is in progress from its header to its last block, and a header that fails
// its CRC starts no packet, so no block is its last.
func TestDataCallInProgress(t *testing.T) {
	t.Parallel()
	for _, badCRC := range []bool{false, true} {
		var c DataCall
		bursts, _ := rigConfirmedRate12Packet(badCRC)
		for i, b := range bursts {
			last := DataBurstEndsCall(&c, b)
			if last != (i == 2 && !badCRC) || c.asm.InProgress() != (i < 2 && !badCRC) {
				t.Errorf("bad CRC %v burst %d: last %v in progress %v", badCRC, i, last, c.asm.InProgress())
			}
		}
	}
}
