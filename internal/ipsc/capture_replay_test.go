package ipsc

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	mmdvm "github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

// Voice calls captured from a MOTOTRBO repeater on a weak signal, taken
// from the ipsc2hbpc test data (github.com/n0mjs710/ipsc2hbpc):
// tests/synclost_ipsc_in.txt lines 1-22, where the repeater loses a burst
// (a 34-byte 0x13 placeholder) and restarts at burst A, and
// tests/lateentry_ipsc_in.txt lines 23-35.
func captureSyncLost() []string {
	return []string{
		"800004c313062f9be50000020200007cdf0080ddae7801f6b057000000000100000a800a00600000000000022f9be50bbebf0011715c",
		"800004c313062f9be50000020200007cdf00805dae7901f6b237000000000100000a800a00600000000000022f9be50bbebf001171a9",
		"800004c313062f9be50000020200007cdf00805dae7a01f6b417000000000a1440781b5fbfa7cb5e07620545e2348781520860e0",
		"800004c313062f9be50000020200007cdf00805dae7b01f6b5f7000000000a1907e818c9ed9eefa205c5ebe334e882f779ecea900616140612",
		"800004c313062f9be50000020200007cdf00805dae7c01f6b7d7000000000a1906a74bb71b7e5cac77ce6ab6126b0cdfd2f21b080b00400616",
		"800004c313062f9be50000020200007cdf00805dae7d01f6b9b7000000000a1906b09a2c2f04bf3026e584d0094c07f0c1f2377848032c0932",
		"800004c313062f9be50000020200007cdf00805dae7e01f6bb97000000000a1907c05168ae4535314b659cd5659c52f092ca58c86ea6333314",
		"800004c313062f9be50000020200007cdf00805dae7f01f6bd77000000000a1906c5363ed900fb315375c49af5cc53e564143520800041002e",
		"800004c313062f9be50000020200007cdf00805dae8001f6bf57000000000a1440c63ddeb18eb235dceca200c79d843b2cdc8048",
		"800004c313062f9be50000020200007cdf00805dae8101f6c137000000000a190fa60e95ae64d9a5e68798c716dc3bdce9eab1280714260612",
		"800004c313062f9be50000020200007cdf00805dae8201f6c317000000000a190e907d5edd4308a41203978a44893441a5e38d202f06000616",
		"800004c313062f9be50000020200007cdf00805dae8301f6c4f7000000000a190e83601e2172fae0dd05ab0514a80601e042ff304c073c0a16",
		"800004c313062f9be50000020200007cdf00805dae8401f6c6d7000000000a229f82a8083862d168ccf2c19bc1f947419b8e834826363a33000000a200022bdef514",
		"800004c313062f9be50000020200007cdf00805dae8501f6c8b7000000000a190f1a6206b2f018641984532c6d0909a9750a14a00004007d10",
		"800004c313062f9be50000020200007cdf00805dae8601f6ca97000000000a144090b481891291600ee9c312dd781d0eb464bb38",
		"800004c313062f9be50000020200007cdf40805eae8701f6cc770000000013000000",
		"800004c313062f9be50000020200007cdf00805dae8801f6ce57000000000a1441a0c6462c06cdac2f60e45a56392445a1b2cc28",
		"800004c313062f9be50000020200007cdf00805dae8901f6d037000000000a1906dd56f880719e6ca2496163edb74fecf617cd900707120612",
		"800004c313062f9be50000020200007cdf00805dae8a01f6d217000000000a19073f1d749395caf7ed75581f237b89ad20e2b5c89f000006e0",
		"800004c313062f9be50000020200007cdf00805dae8b01f6d3f7000000000a1907997d0f836fa36665204abf7a5f1cf11171d5680d033e4906",
		"800004c313062f9be50000020200007cdf40805eae8c01f6d5d7000000000a2297d14a7f3557f461c1b7cc48b8bc2d6aa9ef54401437292b0000680a20c2c9af5a1c",
		"800004c313062f9be50000020200007cdf00805dae8d01f6d7b7000000000a1441d9160b68fd31b511f749bf02fbc96f5b15b190",
	}
}

func captureLateEntry() []string {
	return []string{
		"800004c313052f9be5000002020000430e00805d331469dc5e1d000000000a1440809447044d87202c81e5c0b4080b5969063d20",
		"800004c313052f9be5000002020000430e00805d331569dc5ffd000000000a1906909d07b600a22409868862034923d1e82308380002060612",
		"800004c313052f9be5000002020000430e00805d331669dc61dd000000000a1906923d92591236a593ce326bc04a6bfebbf6b3880f00000616",
		"800004c313052f9be5000002020000430e00805d331769dc63bd000000000a1906a4aef45f0bc1252947ece2c7a91981e6e7d5c82c033c0a16",
		"800004c313052f9be5000002020000430e00805d331869dc659d000000000a190690c70f4c8d1da42d974ba1d48a0af0f2f085800636333310",
		"800004c313052f9be5000002020000430e00805d331969dc677d000000000a1906a0aa9d2ec858a82f4bcfe16d0a0cb9d2e41cf00000000010",
		"800004c313052f9be5000002020000430e00805d331a69dc695d000000000a1440a0c84f220126a833534782b14a0c4269343d50",
		"800004c313052f9be5000002020000430e00805d331b69dc6b3d000000000a1906a0ac472147f52c1be595591aeb63ba41bef7700606069686",
		"800004c313052f9be5000002020000430e00805d331c69dc6d1d000000000a1906b86e7a8fcb96ae0f8a8812e7cae143400363b80f04000616",
		"800004c313052f9be5000002020000430e00805d331d69dc6efd000000000a1906b8e2e1fb505caddf4512ce700c4c514235bf608c033c0a16",
		"800004c313052f9be5000002020000430e00805d331e69dc70dd000000000a2216c1a074198fe5b4240dc418b0ae252978049fa8063631330000000000022f9be514",
		"800004c313052f9be5000002020000430e00805d331f69dc72bd000000000a19069c38571b4fa2398f950dc1e40e830f6a06f7c80080000010",
		"800004c313052f9be5000002020000430e00805d332069dc749d000000000a1440d85bcd46a330b5da90d652872d86eaa01a9dd8",
	}
}

// replayBurstPositions feeds IPSC packets to a fresh translator and returns
// one letter per packet: A-F for a voice burst, H for a voice header, T for
// a terminator, and - for a dropped packet.
func replayBurstPositions(t *testing.T, packets []string) string {
	t.Helper()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatalf("NewIPSCTranslator: %v", err)
	}
	var sb strings.Builder
	for i, h := range packets {
		data, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		out := tr.TranslateToMMDVM(data[0], data)
		switch {
		case len(out) == 0:
			sb.WriteByte('-')
		case out[0].FrameType == mmdvmFrameTypeDataSync && out[0].DTypeOrVSeq == 1:
			sb.WriteByte('H')
		case out[0].FrameType == mmdvmFrameTypeDataSync:
			sb.WriteByte('T')
		case out[0].FrameType == mmdvmFrameTypeVoiceSync:
			sb.WriteByte('A')
		default:
			sb.WriteByte("ABCDEF"[out[0].DTypeOrVSeq%6])
		}
	}
	return sb.String()
}

func TestIPSCVoiceBurstPositionsFromCapture(t *testing.T) {
	t.Parallel()
	withoutA := append(append([]string{}, captureSyncLost()[2:8]...), captureSyncLost()[9:15]...)
	for _, tc := range []struct {
		name    string
		packets []string
		want    string
	}{
		{"burst lost and superframe restarted", captureSyncLost(), "H-ABCDEFABCDEFA-ABCDEA"},
		{"weak-signal call", captureLateEntry(), "ABCDEFABCDEFA"},
		{"late entry before burst E", captureLateEntry()[7:], "---EFA"},
		{"late entry at burst A", captureLateEntry()[6:], "ABCDEFA"},
		{"burst A lost in transit", withoutA, "ABCDEFBCDEFA"},
	} {
		if got := replayBurstPositions(t, tc.packets); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// headerTermDataPackets translates a voice header, terminator, and PI
// header on the given call type and slot to IPSC.
func headerTermDataPackets(t *testing.T, group, slot bool) [][]byte {
	t.Helper()
	tr := newTestTranslator(t)
	data := makeTestMMDVMPacket(group, slot, mmdvmFrameTypeDataSync, uint(enums.DataTypePIHeader))
	data.StreamID = 0x5678
	out := make([][]byte, 0, 5)
	for _, p := range []mmdvm.Packet{
		makeTestMMDVMPacket(group, slot, mmdvmFrameTypeDataSync, uint(enums.DataTypeVoiceLCHeader)),
		makeTestMMDVMPacket(group, slot, mmdvmFrameTypeDataSync, uint(enums.DataTypeTerminatorWithLC)),
		data,
	} {
		out = append(out, tr.TranslateToIPSC(p)...)
	}
	if len(out) < 3 {
		t.Fatalf("group %t slot %t: expected at least 3 packets, got %d", group, slot, len(out))
	}
	return out
}

// MOTOTRBO repeaters send call priority 0x02 in byte 12 for private calls
// too: tests/pvt_ipsc_in.txt in the ipsc2hbpc test data has 0x02 on every
// PVT_VOICE packet.
func TestTranslateToIPSCCallPriority(t *testing.T) {
	t.Parallel()
	for _, group := range []bool{true, false} {
		for i, p := range headerTermDataPackets(t, group, false) {
			if p[12] != 0x02 {
				t.Errorf("group %t packet %d: byte 12 0x%02X, want 0x02", group, i, p[12])
			}
		}
	}
}

// Byte 31 of a header, terminator, or data packet carries the timeslot in
// bit 7, which node-dmr-lib's DMRDataPayload reads as pduSlot. The TS1
// captures in the ipsc2hbpc test data have it clear, and the TS2 capture
// in DMRlink's voice_burst_decoding.txt has 0x80.
func TestTranslateToIPSCSlotFlagInPayload(t *testing.T) {
	t.Parallel()
	for _, slot := range []bool{false, true} {
		want := byte(0x00)
		if slot {
			want = 0x80
		}
		for i, p := range headerTermDataPackets(t, true, slot) {
			if p[31]&0x80 != want {
				t.Errorf("slot %t packet %d: byte 31 0x%02X, want bit 7 = 0x%02X", slot, i, p[31], want)
			}
		}
	}
}

// The repeater sets the call-end bit of byte 17 on weak-signal voice bursts
// in the middle of a call (captureSyncLost lines 15 and 20). Like ipsc2hbpc,
// only a terminator or a non-voice packet ends the stream; a voice burst's
// end bit does not.
func TestIPSCVoiceBurstEndBitKeepsStream(t *testing.T) {
	t.Parallel()
	tr, clock, m := newClockedTranslator(t)
	streams := map[uint]bool{}
	for i, h := range captureSyncLost() {
		data, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		for _, p := range tr.TranslateToMMDVM(data[0], data) {
			streams[p.StreamID] = true
		}
	}
	if len(streams) != 1 {
		t.Fatalf("call split into %d streams, want 1", len(streams))
	}
	if got := activeStreams(t, m, "ipsc_to_mmdvm"); got != 1 {
		t.Fatalf("ipsc_to_mmdvm gauge %v, want 1", got)
	}

	// The capture has no terminator; pruning still ends the call.
	clock.t = clock.t.Add(streamIdleTimeout + time.Second)
	d := makeTestIPSCPacket(0x83, ipscBurstCSBK, true, false)
	tr.TranslateToMMDVM(0x83, d)
	if len(tr.reverseStreams) != 1 {
		t.Fatalf("expected only the new stream after pruning, got %d", len(tr.reverseStreams))
	}

	data := makeTestIPSCPacket(0x83, ipscBurstCSBK, true, false)
	data[17] |= 0x40
	binary.BigEndian.PutUint32(data[13:17], 0xD00D)
	tr.TranslateToMMDVM(0x83, data)
	if _, ok := tr.reverseStreams[0xD00D]; ok {
		t.Fatal("a data packet with the end bit did not end its stream")
	}
}

// In MOTOTRBO captures the embedded LC fragment of bursts B-E is bytes
// 52-55 and the EMB is the last byte: on the clean private call in
// ipsc2hbpc's tests/pvt_ipsc_in.txt the four fragments decode to the LC
// that burst E repeats in bytes 56-64. On a weak signal the repeater sends
// LCSS values that do not match the position (captureSyncLost line 5 is
// burst D with a first-fragment LCSS), so the LCSS follows the position.
func TestIPSCEmbeddedSignallingFromCapture(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	for i, h := range captureSyncLost() {
		data, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		out := tr.TranslateToMMDVM(data[0], data)
		if len(out) != 1 || out[0].FrameType != mmdvmFrameTypeVoice {
			continue
		}
		pos := int(out[0].DTypeOrVSeq) //nolint:gosec // a voice sequence is 1-5
		raw := layer2.RawBurst(out[0].DMRData)
		emb := raw.EMB()
		if got := emb.LCSS; got != defaultLCSS(pos) {
			t.Errorf("packet %d burst %c: LCSS %d, want %d", i, 'A'+pos, got, defaultLCSS(pos))
		}
		if got := raw.EmbeddedData(); string(got[:]) != string(data[52:56]) {
			t.Errorf("packet %d burst %c: fragment % X, want % X", i, 'A'+pos, got, data[52:56])
		}
	}
}
