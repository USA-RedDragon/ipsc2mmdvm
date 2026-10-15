package ipsc

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/vocoder"
	mmdvm "github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

func rawEMB(t *testing.T, raw *layer2.RawBurst) pdu.EmbeddedSignalling {
	t.Helper()
	emb := raw.EMB()
	if emb.FEC.Uncorrectable {
		t.Fatal("expected a valid EMB")
	}
	return emb
}

func newTestTranslator(t *testing.T) *IPSCTranslator {
	t.Helper()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatalf("NewIPSCTranslator() error: %v", err)
	}
	tr.SetPeerID(12345)
	return tr
}

func TestNewIPSCTranslator(t *testing.T) {
	t.Parallel()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatalf("NewIPSCTranslator() error: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil translator")
	}
	if tr.streams == nil {
		t.Fatal("expected non-nil streams map")
	}
	if tr.reverseStreams == nil {
		t.Fatal("expected non-nil reverseStreams map")
	}
}

func TestSetPeerID(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	if tr.peerID != 12345 {
		t.Fatalf("expected peerID 12345, got %d", tr.peerID)
	}
	if tr.repeaterID != 12345 {
		t.Fatalf("expected repeaterID 12345, got %d", tr.repeaterID)
	}
}

func TestCleanupStream(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Create some stream state by translating a voice header
	pkt := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1) // VoiceLCHeader=1
	tr.TranslateToIPSC(pkt)

	streamID := uint32(pkt.StreamID) //nolint:gosec // test value is within uint32 range

	tr.mu.Lock()
	_, exists := tr.streams[streamID]
	tr.mu.Unlock()

	if !exists {
		t.Fatal("expected stream state to exist after translate")
	}

	tr.CleanupStream(streamID)

	tr.mu.Lock()
	_, exists = tr.streams[streamID]
	tr.mu.Unlock()

	if exists {
		t.Fatal("expected stream state to be removed after cleanup")
	}
}

func makeTestMMDVMPacket(groupCall, slot bool, frameType, dtypeOrVSeq uint) mmdvm.Packet {
	return mmdvm.Packet{
		Signature:   mmdvm.DMRDSignature,
		Seq:         0,
		Src:         100,
		Dst:         200,
		Repeater:    3001,
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   frameType,
		DTypeOrVSeq: dtypeOrVSeq,
		StreamID:    0x1234,
		DMRData:     [33]byte{},
	}
}

func TestTranslateToIPSCNilOnUnknownFrameType(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, 3, 0) // frameType=3 is unknown
	result := tr.TranslateToIPSC(pkt)
	if result != nil {
		t.Fatalf("expected nil for unknown frame type, got %d packets", len(result))
	}
}

func TestTranslateToIPSCVoiceHeaderProduces3Packets(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	// DataTypeVoiceLCHeader = 1
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) != 3 {
		t.Fatalf("expected 3 voice header packets, got %d", len(result))
	}
}

func TestTranslateToIPSCVoiceTerminator(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	// First send a header to establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// DataTypeTerminatorWithLC = 2
	term := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 2)
	term.StreamID = header.StreamID
	result := tr.TranslateToIPSC(term)
	if len(result) != 1 {
		t.Fatalf("expected 1 terminator packet, got %d", len(result))
	}
}

func TestTranslateToIPSCGroupCallFlag(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Group call
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	if result[0][0] != 0x80 {
		t.Fatalf("expected group voice type 0x80, got 0x%02X", result[0][0])
	}

	// Private call
	tr2 := newTestTranslator(t)
	pkt2 := makeTestMMDVMPacket(false, false, mmdvmFrameTypeDataSync, 1)
	pkt2.StreamID = 0x5678
	result2 := tr2.TranslateToIPSC(pkt2)
	if len(result2) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	if result2[0][0] != 0x81 {
		t.Fatalf("expected private voice type 0x81, got 0x%02X", result2[0][0])
	}
}

func TestTranslateToIPSCPeerIDInHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	peerID := binary.BigEndian.Uint32(result[0][1:5])
	if peerID != 12345 {
		t.Fatalf("expected peer ID 12345 in header, got %d", peerID)
	}
}

func TestTranslateToIPSCSlotFlag(t *testing.T) {
	t.Parallel()

	// TS1 (Slot=false)
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected packets")
	}
	callInfo := result[0][17]
	if callInfo&0x20 != 0 {
		t.Fatalf("expected TS1 (slot bit clear), got callInfo %02X", callInfo)
	}

	// TS2 (Slot=true)
	tr2 := newTestTranslator(t)
	pkt2 := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1)
	pkt2.StreamID = 0x9999
	result2 := tr2.TranslateToIPSC(pkt2)
	if len(result2) < 1 {
		t.Fatal("expected packets")
	}
	callInfo2 := result2[0][17]
	if callInfo2&0x20 == 0 {
		t.Fatalf("expected TS2 (slot bit set), got callInfo %02X", callInfo2)
	}
}

func TestTranslateToIPSCSrcDstInHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	pkt.Src = 0x123456
	pkt.Dst = 0xABCDEF
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected packets")
	}
	src := uint(result[0][6])<<16 | uint(result[0][7])<<8 | uint(result[0][8])
	dst := uint(result[0][9])<<16 | uint(result[0][10])<<8 | uint(result[0][11])
	if src != 0x123456 {
		t.Fatalf("expected src 0x123456, got 0x%06X", src)
	}
	if dst != 0xABCDEF {
		t.Fatalf("expected dst 0xABCDEF, got 0x%06X", dst)
	}
}

func TestTranslateToMMDVMTooShort(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	result := tr.TranslateToMMDVM(0x80, make([]byte, 10))
	if result != nil {
		t.Fatal("expected nil for too-short IPSC packet")
	}
}

func TestTranslateToMMDVMUnsupportedType(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	result := tr.TranslateToMMDVM(0x99, make([]byte, 54))
	if result != nil {
		t.Fatal("expected nil for unsupported packet type")
	}
}

func makeTestIPSCPacket(packetType byte, burstType byte, groupCall, slot bool) []byte {
	buf := make([]byte, 54)
	buf[0] = packetType

	// Peer ID
	binary.BigEndian.PutUint32(buf[1:5], 99999)

	// Src (bytes 6-8) — hardcoded test source ID
	const src uint = 100
	const dst uint = 200
	buf[6] = byte(src >> 16)
	buf[7] = byte(src >> 8)
	buf[8] = byte(src)

	// Dst (bytes 9-11)
	buf[9] = byte(dst >> 16)
	buf[10] = byte(dst >> 8)
	buf[11] = byte(dst)

	// Call type
	if groupCall {
		buf[12] = 0x02
	} else {
		buf[12] = 0x01
	}

	// Call control (bytes 13-16) - unique per call
	binary.BigEndian.PutUint32(buf[13:17], 0xAAAA)

	// Call info (byte 17)
	callInfo := byte(0x00)
	if slot {
		callInfo |= 0x20
	}
	buf[17] = callInfo

	// RTP header stub (bytes 18-29)
	buf[18] = 0x80

	// Burst type (byte 30)
	buf[30] = burstType

	return buf
}

func TestTranslateToMMDVMVoiceHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice header, got %d", len(result))
	}
	pkt := result[0]
	if pkt.Signature != mmdvm.DMRDSignature {
		t.Fatalf("expected DMRD signature, got %q", pkt.Signature)
	}
	if pkt.FrameType != mmdvmFrameTypeDataSync {
		t.Fatalf("expected frame type %d (data sync), got %d", mmdvmFrameTypeDataSync, pkt.FrameType)
	}
	if pkt.Src != 100 {
		t.Fatalf("expected src 100, got %d", pkt.Src)
	}
	if pkt.Dst != 200 {
		t.Fatalf("expected dst 200, got %d", pkt.Dst)
	}
}

func TestTranslateToMMDVMDuplicateHeaderSkipped(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)

	// First header should produce a packet
	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for first header, got %d", len(result))
	}

	// Second header with same call control should be skipped
	result = tr.TranslateToMMDVM(0x80, data)
	if len(result) != 0 {
		t.Fatalf("expected 0 packets for duplicate header, got %d", len(result))
	}
}

func TestTranslateToMMDVMVoiceTerminator(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send header first to establish stream
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	tr.TranslateToMMDVM(0x80, header)

	// Send terminator
	term := makeTestIPSCPacket(0x80, ipscBurstVoiceTerm, true, false)
	result := tr.TranslateToMMDVM(0x80, term)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for terminator, got %d", len(result))
	}
	if result[0].DTypeOrVSeq != 2 { // DataTypeTerminatorWithLC = 2
		t.Fatalf("expected dtype 2 (terminator), got %d", result[0].DTypeOrVSeq)
	}
}

func TestTranslateToMMDVMPrivateCall(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x81, ipscBurstVoiceHead, false, false)
	result := tr.TranslateToMMDVM(0x81, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if result[0].GroupCall {
		t.Fatal("expected GroupCall=false for private call")
	}
}

func TestTranslateToMMDVMSlotTS2(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, true)
	// Use a different call control to avoid collision
	binary.BigEndian.PutUint32(data[13:17], 0xBBBB)
	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if !result[0].Slot {
		t.Fatal("expected Slot=true for TS2")
	}
}

func TestTranslateToMMDVMEndFlagCleansUp(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send header
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xCCCC)
	tr.TranslateToMMDVM(0x80, header)

	// Send another packet with end flag set (but not a terminator burst type)
	endPkt := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(endPkt[13:17], 0xCCCC)
	endPkt[17] |= 0x40 // set end flag
	tr.TranslateToMMDVM(0x80, endPkt)

	// Verify the stream was cleaned up
	tr.mu.Lock()
	_, exists := tr.reverseStreams[0xCCCC]
	tr.mu.Unlock()
	if exists {
		t.Fatal("expected reverse stream to be cleaned up after end flag")
	}
}

func TestTranslateToMMDVMCSBK(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x83, ipscBurstCSBK, true, false)
	binary.BigEndian.PutUint32(data[13:17], 0xDDDD)
	result := tr.TranslateToMMDVM(0x83, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for CSBK, got %d", len(result))
	}
	if result[0].DTypeOrVSeq != 3 { // DataTypeCSBK = 3
		t.Fatalf("expected dtype 3 (CSBK), got %d", result[0].DTypeOrVSeq)
	}
}

func TestExtractFullLCBytesGroupCall(t *testing.T) {
	t.Parallel()
	pkt := mmdvm.Packet{
		GroupCall: true,
		Src:       100,
		Dst:       200,
	}
	lc := extractFullLCBytes(pkt)
	// First byte should be FLCO for group call (0x00)
	if lc[0] != 0x00 {
		t.Fatalf("expected FLCO 0x00 (group), got 0x%02X", lc[0])
	}
}

func TestExtractFullLCBytesPrivateCall(t *testing.T) {
	t.Parallel()
	pkt := mmdvm.Packet{
		GroupCall: false,
		Src:       100,
		Dst:       200,
	}
	lc := extractFullLCBytes(pkt)
	// First byte should be FLCO for unit-to-unit (0x03)
	if lc[0] != 0x03 {
		t.Fatalf("expected FLCO 0x03 (unit-to-unit), got 0x%02X", lc[0])
	}
}

func TestBuildIPSCHeaderDataPacket(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 3) // CSBK
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 data packet")
	}
	// Data packet should use type 0x83 (group data)
	if result[0][0] != 0x83 {
		t.Fatalf("expected data packet type 0x83, got 0x%02X", result[0][0])
	}
}

func TestBuildIPSCHeaderEndFlag(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	// First send a header
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Then send terminator (end flag should be set)
	term := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 2)
	term.StreamID = header.StreamID
	result := tr.TranslateToIPSC(term)
	if len(result) != 1 {
		t.Fatalf("expected 1 terminator packet, got %d", len(result))
	}
	callInfo := result[0][17]
	if callInfo&0x40 == 0 {
		t.Fatalf("expected end flag set in terminator, got callInfo %02X", callInfo)
	}
}

func TestBuildRTPHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	// RTP version should be 2 (0x80 = version 2, no padding, no ext, 0 CSRCs)
	if result[0][18] != 0x80 {
		t.Fatalf("expected RTP version byte 0x80, got 0x%02X", result[0][18])
	}
}

func TestBuildRTPHeaderNoMarker(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 3 {
		t.Fatal("expected 3 header packets")
	}
	// Third header should not have marker bit
	pt := result[2][19]
	if pt&0x80 != 0 {
		t.Fatalf("expected no marker on third header, got PT byte 0x%02X", pt)
	}
}

func TestMultipleStreamsConcurrent(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Start two separate streams
	pkt1 := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	pkt1.StreamID = 0xAAAA
	pkt2 := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1)
	pkt2.StreamID = 0xBBBB

	result1 := tr.TranslateToIPSC(pkt1)
	result2 := tr.TranslateToIPSC(pkt2)

	if len(result1) != 3 {
		t.Fatalf("stream 1: expected 3 packets, got %d", len(result1))
	}
	if len(result2) != 3 {
		t.Fatalf("stream 2: expected 3 packets, got %d", len(result2))
	}

	// Each stream should have its own call control
	cc1 := binary.BigEndian.Uint32(result1[0][13:17])
	cc2 := binary.BigEndian.Uint32(result2[0][13:17])
	if cc1 == cc2 {
		t.Fatal("expected different call control values for different streams")
	}
}

// makeVoiceDMRData builds a 33-byte DMR voice burst (with sync pattern) that
// round-trips through layer2.Burst Decode/Encode. This allows buildVoiceBurst
// and buildMMDVMVoiceBurst to work on realistic data.
func makeVoiceDMRData(syncBurst bool) [33]byte {
	// Minimal voice frames: all-zero vocoder slots
	burst := layer2.Burst{Kind: layer2.KindVoice}
	if syncBurst {
		burst.SyncPattern = enums.SyncMSSourcedVoice
		burst.VoiceBurst = enums.VoiceBurstA
	} else {
		burst.SyncPattern = enums.SyncNone
		burst.VoiceBurst = enums.VoiceBurstB
		burst.EmbeddedSignalling = pdu.EmbeddedSignalling{
			ColorCode:                          0,
			PreemptionAndPowerControlIndicator: false,
			LCSS:                               enums.LCSSFirstFragment,
		}
	}
	data, err := burst.Encode()
	if err != nil {
		panic(err)
	}
	return data
}

func TestBuildVoiceBurstA(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send a header first to establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Build a voice sync burst (burst A, index 0)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoiceSync, 0)
	pkt.StreamID = header.StreamID
	pkt.DMRData = makeVoiceDMRData(true)

	result := tr.TranslateToIPSC(pkt)
	if len(result) != 1 {
		t.Fatalf("expected 1 voice burst packet, got %d", len(result))
	}

	// Burst A should be 52 bytes
	if len(result[0]) != 52 {
		t.Fatalf("expected burst A to be 52 bytes, got %d", len(result[0]))
	}

	// Check the slot type byte
	slotByte := result[0][30]
	if slotByte != ipscBurstSlot1 {
		t.Fatalf("expected slot1 burst type 0x%02X, got 0x%02X", ipscBurstSlot1, slotByte)
	}

	// Check length byte
	if result[0][31] != 0x14 {
		t.Fatalf("expected length byte 0x14, got 0x%02X", result[0][31])
	}

	// Unknown field byte
	if result[0][32] != 0x40 {
		t.Fatalf("expected unknown field 0x40, got 0x%02X", result[0][32])
	}
}

func TestBuildVoiceBurstBCDF(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send a header to establish stream state
	header := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Send burst A to advance burstIndex to 1
	burstA := makeTestMMDVMPacket(true, true, mmdvmFrameTypeVoiceSync, 0)
	burstA.StreamID = header.StreamID
	burstA.DMRData = makeVoiceDMRData(true)
	tr.TranslateToIPSC(burstA)

	// Now send burst B (burstIndex=1) — should produce 57-byte packet
	burstB := makeTestMMDVMPacket(true, true, mmdvmFrameTypeVoice, 1)
	burstB.StreamID = header.StreamID
	burstB.DMRData = makeVoiceDMRData(false)

	result := tr.TranslateToIPSC(burstB)
	if len(result) != 1 {
		t.Fatalf("expected 1 voice burst packet, got %d", len(result))
	}

	// Bursts B,C,D,F should be 57 bytes
	if len(result[0]) != 57 {
		t.Fatalf("expected burst B to be 57 bytes, got %d", len(result[0]))
	}

	// Check slot type byte — TS2
	slotByte := result[0][30]
	if slotByte != ipscBurstSlot2 {
		t.Fatalf("expected slot2 burst type 0x%02X, got 0x%02X", ipscBurstSlot2, slotByte)
	}

	// Check length byte
	if result[0][31] != 0x19 {
		t.Fatalf("expected length byte 0x19, got 0x%02X", result[0][31])
	}

	// Unknown field
	if result[0][32] != 0x06 {
		t.Fatalf("expected unknown field 0x06, got 0x%02X", result[0][32])
	}
}

func TestBuildVoiceBurstE(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Send bursts A-D to advance burstIndex to 4
	for i := 0; i < 4; i++ {
		ft := mmdvmFrameTypeVoice
		if i == 0 {
			ft = mmdvmFrameTypeVoiceSync
		}
		pkt := makeTestMMDVMPacket(true, false, ft, uint(i)) //nolint:gosec // G115: i is in [0,3]
		pkt.StreamID = header.StreamID
		pkt.DMRData = makeVoiceDMRData(i == 0)
		tr.TranslateToIPSC(pkt)
	}

	// Now send burst E (burstIndex=4) — should produce 66-byte packet
	burstE := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoice, 4)
	burstE.StreamID = header.StreamID
	burstE.DMRData = makeVoiceDMRData(false)
	burstE.Src = 0x112233
	burstE.Dst = 0x445566

	result := tr.TranslateToIPSC(burstE)
	if len(result) != 1 {
		t.Fatalf("expected 1 voice burst packet, got %d", len(result))
	}

	// Burst E should be 66 bytes
	if len(result[0]) != 66 {
		t.Fatalf("expected burst E to be 66 bytes, got %d", len(result[0]))
	}

	// Check length byte
	if result[0][31] != 0x22 {
		t.Fatalf("expected length byte 0x22 for burst E, got 0x%02X", result[0][31])
	}

	// Unknown field
	if result[0][32] != 0x16 {
		t.Fatalf("expected unknown field 0x16 for burst E, got 0x%02X", result[0][32])
	}

	// Check that dst is repeated at bytes 59-61
	dstRepeated := uint(result[0][59])<<16 | uint(result[0][60])<<8 | uint(result[0][61])
	if dstRepeated != 0x445566 {
		t.Fatalf("expected dst 0x445566 at bytes 59-61, got 0x%06X", dstRepeated)
	}

	// Check that src is repeated at bytes 62-64
	srcRepeated := uint(result[0][62])<<16 | uint(result[0][63])<<8 | uint(result[0][64])
	if srcRepeated != 0x112233 {
		t.Fatalf("expected src 0x112233 at bytes 62-64, got 0x%06X", srcRepeated)
	}

	// Trailer byte
	if result[0][65] != 0x14 {
		t.Fatalf("expected trailer byte 0x14, got 0x%02X", result[0][65])
	}
}

// TestBuildVoiceBurstTrustsFrameType checks that a voice burst is decoded as
// voice by its DMRD frame type, whatever its SYNC field holds. The burst is
// a real MMDVM voice burst D whose embedded signalling field equals the MS
// RC SYNC pattern, which a SYNC classification takes for a standalone RC
// burst with no voice.
func TestBuildVoiceBurstTrustsFrameType(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	dmrData := [33]byte{
		0x08, 0x5d, 0xc1, 0x1e, 0x5e, 0x1d, 0x32, 0x8f, 0x0c, 0xae, 0x4b,
		0xc9, 0x1d, 0xa7, 0x7d, 0x55, 0xf7, 0xdf, 0xd7, 0x7a, 0xb8, 0x08,
		0xe7, 0x0a, 0x09, 0x6a, 0xd3, 0x2d, 0xe1, 0xf5, 0x6d, 0x0f, 0x9e,
	}
	var bySync layer2.Burst
	_ = bySync.DecodeFromBytes(dmrData)
	if bySync.Kind != layer2.KindReverseChannel {
		t.Fatalf("expected the SYNC field to classify the burst as RC, got %s", bySync.Kind)
	}

	var voice layer2.Burst
	if err := voice.DecodeFromBytesWith(dmrData, &layer2.DecodeOptions{
		Kind: layer2.KindVoice, VoiceBurst: enums.VoiceBurstD,
	}); err != nil {
		t.Fatal(err)
	}
	frames, res := voice.AMBEFrames()
	if res.Uncorrectable {
		t.Fatalf("expected the voice frames to decode, got %+v", res)
	}
	want := vocoder.PackAMBEVoice(frames)
	if want == ([19]byte{}) {
		t.Fatal("expected nonzero voice")
	}

	for vseq := uint(1); vseq <= 3; vseq++ {
		pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoice, vseq)
		pkt.StreamID = header.StreamID
		pkt.DMRData = makeVoiceDMRData(false)
		if vseq == 3 {
			pkt.DMRData = dmrData
		}
		result := tr.TranslateToIPSC(pkt)
		if len(result) != 1 {
			t.Fatalf("vseq %d: expected 1 packet, got %d", vseq, len(result))
		}
		if vseq == 3 && [19]byte(result[0][33:52]) != want {
			t.Fatalf("expected AMBE % X, got % X", want, result[0][33:52])
		}
	}
}

func TestBuildVoiceBurstWrapsAfterF(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Send 6 bursts (A-F) to complete one superframe
	for i := 0; i < 6; i++ {
		ft := mmdvmFrameTypeVoice
		if i == 0 {
			ft = mmdvmFrameTypeVoiceSync
		}
		pkt := makeTestMMDVMPacket(true, false, ft, uint(i)) //nolint:gosec // G115: i is in [0,5]
		pkt.StreamID = header.StreamID
		pkt.DMRData = makeVoiceDMRData(i == 0)
		tr.TranslateToIPSC(pkt)
	}

	// The 7th burst should wrap to index 0 (burst A again) → 52 bytes
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoiceSync, 0)
	pkt.StreamID = header.StreamID
	pkt.DMRData = makeVoiceDMRData(true)

	result := tr.TranslateToIPSC(pkt)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if len(result[0]) != 52 {
		t.Fatalf("expected 52 bytes (burst A after wrap), got %d", len(result[0]))
	}
}

func TestBuildMMDVMVoiceBurstFromSlot1(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send header to establish reverse stream
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	tr.TranslateToMMDVM(0x80, header)

	// Build an IPSC voice burst (slot1 = burst A, 52 bytes)
	burstData := make([]byte, 52)
	copy(burstData[:18], header[:18]) // reuse IPSC header
	burstData[30] = ipscBurstSlot1
	burstData[31] = 0x14
	burstData[32] = 0x40
	// AMBE data at bytes 33-51 (19 bytes, zeros = silence)

	result := tr.TranslateToMMDVM(0x80, burstData)
	if len(result) != 1 {
		t.Fatalf("expected 1 MMDVM packet for voice burst, got %d", len(result))
	}

	pkt := result[0]
	if pkt.Signature != mmdvm.DMRDSignature {
		t.Fatalf("expected DMRD signature, got %q", pkt.Signature)
	}
	if pkt.Src != 100 {
		t.Fatalf("expected src 100, got %d", pkt.Src)
	}
	if pkt.Dst != 200 {
		t.Fatalf("expected dst 200, got %d", pkt.Dst)
	}
	if !pkt.GroupCall {
		t.Fatal("expected GroupCall=true")
	}
	if pkt.Slot {
		t.Fatal("expected Slot=false for TS1")
	}
	// First voice burst (burstIndex=0) should be voice sync
	if pkt.FrameType != mmdvmFrameTypeVoiceSync {
		t.Fatalf("expected frame type %d (voice sync), got %d", mmdvmFrameTypeVoiceSync, pkt.FrameType)
	}
	if pkt.DTypeOrVSeq != 0 {
		t.Fatalf("expected DTypeOrVSeq 0 (burst A), got %d", pkt.DTypeOrVSeq)
	}
}

func TestBuildMMDVMVoiceBurstSequencing(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish reverse stream with a header
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xEEEE)
	tr.TranslateToMMDVM(0x80, header)

	// Send 3 voice bursts and verify sequencing
	for i := 0; i < 3; i++ {
		burstData := make([]byte, 52)
		copy(burstData[:18], header[:18])
		binary.BigEndian.PutUint32(burstData[13:17], 0xEEEE)
		burstData[30] = ipscBurstSlot1
		burstData[31] = 0x14
		burstData[32] = 0x40

		result := tr.TranslateToMMDVM(0x80, burstData)
		if len(result) != 1 {
			t.Fatalf("burst %d: expected 1 packet, got %d", i, len(result))
		}

		pkt := result[0]
		if pkt.DTypeOrVSeq != uint(i) {
			t.Fatalf("burst %d: expected DTypeOrVSeq %d, got %d", i, i, pkt.DTypeOrVSeq)
		}
		// Burst 0 = voice sync, rest = voice
		if i == 0 {
			if pkt.FrameType != mmdvmFrameTypeVoiceSync {
				t.Fatalf("burst 0: expected voice sync frame type, got %d", pkt.FrameType)
			}
		} else {
			if pkt.FrameType != mmdvmFrameTypeVoice {
				t.Fatalf("burst %d: expected voice frame type, got %d", i, pkt.FrameType)
			}
		}
		if pkt.Seq != uint(i+1) { //nolint:gosec // G115: i is in [0,2]; seq starts at 1 after header consumed seq=0
			t.Fatalf("burst %d: expected seq %d, got %d", i, i+1, pkt.Seq)
		}
	}
}

func TestBuildMMDVMVoiceBurstWrapsAt6(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xFFFF)
	tr.TranslateToMMDVM(0x80, header)

	// Send 7 voice bursts — the 7th should wrap to burstIndex 0 (voice sync again)
	for i := 0; i < 7; i++ {
		burstData := make([]byte, 52)
		copy(burstData[:18], header[:18])
		binary.BigEndian.PutUint32(burstData[13:17], 0xFFFF)
		burstData[30] = ipscBurstSlot1
		burstData[31] = 0x14
		burstData[32] = 0x40

		result := tr.TranslateToMMDVM(0x80, burstData)
		if len(result) != 1 {
			t.Fatalf("burst %d: expected 1 packet, got %d", i, len(result))
		}

		pkt := result[0]
		expectedIdx := i % 6
		//nolint:gosec // G115: i is in [0,6], expectedIdx fits in uint
		if pkt.DTypeOrVSeq != uint(expectedIdx) {
			t.Fatalf("burst %d: expected DTypeOrVSeq %d, got %d", i, expectedIdx, pkt.DTypeOrVSeq)
		}
	}
}

func TestBuildMMDVMVoiceBurstSlot2(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, true)
	binary.BigEndian.PutUint32(header[13:17], 0x1111)
	tr.TranslateToMMDVM(0x80, header)

	burstData := make([]byte, 52)
	copy(burstData[:18], header[:18])
	binary.BigEndian.PutUint32(burstData[13:17], 0x1111)
	burstData[30] = ipscBurstSlot2
	burstData[31] = 0x14
	burstData[32] = 0x40

	result := tr.TranslateToMMDVM(0x80, burstData)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if !result[0].Slot {
		t.Fatal("expected Slot=true for TS2 voice burst")
	}
}

func TestBuildMMDVMVoiceBurstTooShort(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish reverse stream
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0x2222)
	tr.TranslateToMMDVM(0x80, header)

	// Send a voice burst packet that is too short (< 52 bytes)
	burstData := make([]byte, 40)
	copy(burstData[:18], header[:18])
	binary.BigEndian.PutUint32(burstData[13:17], 0x2222)
	burstData[30] = ipscBurstSlot1

	result := tr.TranslateToMMDVM(0x80, burstData)
	if result != nil {
		t.Fatalf("expected nil for too-short voice burst, got %d packets", len(result))
	}
}

func TestBuildMMDVMVoiceBurstPrivateCall(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x81, ipscBurstVoiceHead, false, false)
	binary.BigEndian.PutUint32(header[13:17], 0x3333)
	tr.TranslateToMMDVM(0x81, header)

	burstData := make([]byte, 52)
	copy(burstData[:18], header[:18])
	binary.BigEndian.PutUint32(burstData[13:17], 0x3333)
	burstData[30] = ipscBurstSlot1
	burstData[31] = 0x14
	burstData[32] = 0x40

	result := tr.TranslateToMMDVM(0x81, burstData)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if result[0].GroupCall {
		t.Fatal("expected GroupCall=false for private call voice burst")
	}
}

func TestPopulateEmbeddedSignallingBurstB(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var raw layer2.RawBurst

	// Burst B with 57-byte packet (5 bytes embedded data at [52:57])
	ipscData := make([]byte, 57)
	ipscData[52] = 0xAB
	ipscData[53] = 0xCD
	ipscData[54] = 0xEF
	ipscData[55] = 0x12

	tr.populateEmbeddedSignalling(&raw, 1, ipscData)

	// Burst B (index 1) should have LCSS = FirstFragmentLC
	if rawEMB(t, &raw).LCSS != enums.LCSSFirstFragment {
		t.Fatalf("expected LCSS FirstFragmentLC, got %d", rawEMB(t, &raw).LCSS)
	}

	// Check that embedded data was unpacked
	packed := raw.EmbeddedData()
	//nolint:gosec // packed is [4]byte, index is always in range
	if packed[0] != 0xAB || packed[1] != 0xCD || packed[2] != 0xEF || packed[3] != 0x12 {
		t.Fatalf("expected embedded data [AB CD EF 12], got [%02X %02X %02X %02X]",
			packed[0], packed[1], packed[2], packed[3])
	}
}

func TestPopulateEmbeddedSignallingBurstE(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var raw layer2.RawBurst

	// Burst E with 66-byte packet (7 bytes embedded data at [52:59])
	ipscData := make([]byte, 66)
	ipscData[52] = 0x11
	ipscData[53] = 0x22
	ipscData[54] = 0x33
	ipscData[55] = 0x44

	tr.populateEmbeddedSignalling(&raw, 4, ipscData)

	// Burst E (index 4) should have LCSS = LastFragmentLCorCSBK
	if rawEMB(t, &raw).LCSS != enums.LCSSLastFragment {
		t.Fatalf("expected LCSS LastFragmentLCorCSBK, got %d", rawEMB(t, &raw).LCSS)
	}

	packed := raw.EmbeddedData()
	//nolint:gosec // packed is [4]byte, index is always in range
	if packed[0] != 0x11 || packed[1] != 0x22 || packed[2] != 0x33 || packed[3] != 0x44 {
		t.Fatalf("expected embedded data [11 22 33 44], got [%02X %02X %02X %02X]",
			packed[0], packed[1], packed[2], packed[3])
	}
}

func TestPopulateEmbeddedSignallingContinuation(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var raw layer2.RawBurst

	// Bursts C, D, F should all get ContinuationFragmentLCorCSBK
	for _, idx := range []int{2, 3, 5} {
		ipscData := make([]byte, 57)
		tr.populateEmbeddedSignalling(&raw, idx, ipscData)
		if rawEMB(t, &raw).LCSS != enums.LCSSContinuationFragment {
			t.Fatalf("burst index %d: expected LCSS ContinuationFragmentLCorCSBK, got %d",
				idx, rawEMB(t, &raw).LCSS)
		}
	}
}

func TestPopulateEmbeddedSignallingNoEmbeddedData(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var raw layer2.RawBurst

	// A 52-byte packet doesn't match 57 or 66, so no embedded data is extracted
	ipscData := make([]byte, 52)
	tr.populateEmbeddedSignalling(&raw, 1, ipscData)

	// LCSS should still be set
	if rawEMB(t, &raw).LCSS != enums.LCSSFirstFragment {
		t.Fatalf("expected LCSS FirstFragmentLC, got %d", rawEMB(t, &raw).LCSS)
	}

	// Embedded data should remain empty (all zeros)
	packed := raw.EmbeddedData()
	for i, b := range packed {
		if b != 0 {
			t.Fatalf("expected zero embedded data byte %d, got 0x%02X", i, b)
		}
	}
}

func TestTranslateToMMDVMBoundsAllLengths(t *testing.T) {
	t.Parallel()
	burstTypes := []byte{
		ipscBurstVoiceHead, ipscBurstVoiceTerm, ipscBurstCSBK,
		ipscBurstSlot1, ipscBurstSlot2, 0x00, 0x06, 0x07, 0x08, 0x09, 0x0B, 0x3F, 0xFF,
	}
	for _, packetType := range []byte{0x80, 0x81, 0x83, 0x84} {
		for _, burstType := range burstTypes {
			for n := 0; n <= 80; n++ {
				tr := newTestTranslator(t)
				data := make([]byte, n)
				for i := range data {
					data[i] = byte(i*31 + 7)
				}
				if n > 30 {
					data[30] = burstType
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic for type 0x%02X burst 0x%02X len %d: %v", packetType, burstType, n, r)
						}
					}()
					tr.TranslateToMMDVM(packetType, data)
				}()
			}
		}
	}
}

func TestTranslateToMMDVM30BytePacket(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := make([]byte, 30)
	data[0] = 0x80
	if got := tr.TranslateToMMDVM(0x80, data); got != nil {
		t.Fatalf("expected nil for 30-byte packet, got %d packets", len(got))
	}
}

// reservedSlotTypeBurst returns a data burst whose slot type carries the
// reserved data type dt.
func reservedSlotTypeBurst(tb testing.TB, dt enums.DataType) [33]byte {
	tb.Helper()
	burst, err := layer2.BuildDataBurst([12]byte{}, enums.DataTypeCSBK, 0, layer2.SyncSourceBS)
	if err != nil {
		tb.Fatal(err)
	}
	burst.SetSlotType(pdu.SlotType{DataType: dt})
	return burst
}

func TestTranslateToIPSCReservedSlotTypeDoesNotPanic(t *testing.T) {
	t.Parallel()
	for dt := 12; dt < 16; dt++ {
		burst := reservedSlotTypeBurst(t, enums.DataType(dt)) //nolint:gosec // dt is in [12,15]
		for _, ft := range []uint{mmdvmFrameTypeVoice, mmdvmFrameTypeVoiceSync, mmdvmFrameTypeDataSync} {
			for _, dtype := range []uint{0, 1, 2, 3, 4, 5, 6, 7, 8, 10} {
				tr := newTestTranslator(t)
				hdr := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
				tr.TranslateToIPSC(hdr)
				pkt := makeTestMMDVMPacket(true, false, ft, dtype)
				pkt.DMRData = burst
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic for slot type %d frameType %d dtype %d: %v", dt, ft, dtype, r)
						}
					}()
					tr.TranslateToIPSC(pkt)
				}()
			}
		}
	}
}

func FuzzTranslateToMMDVM(f *testing.F) {
	f.Add(byte(0x80), make([]byte, 30))
	f.Add(byte(0x80), make([]byte, 31))
	f.Add(byte(0x83), makeTestIPSCPacket(0x83, ipscBurstCSBK, true, false))
	f.Add(byte(0x80), makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false))
	f.Add(byte(0x80), makeTestIPSCPacket(0x80, ipscBurstVoiceTerm, true, true))
	f.Add(byte(0x80), append(makeTestIPSCPacket(0x80, ipscBurstSlot1, true, false), make([]byte, 12)...))
	f.Fuzz(func(t *testing.T, packetType byte, data []byte) {
		tr := newTestTranslator(t)
		for i := 0; i < 3; i++ {
			tr.TranslateToMMDVM(packetType, data)
		}
	})
}

func FuzzTranslateToIPSC(f *testing.F) {
	f.Add(uint8(1), uint8(1), true, false, make([]byte, 33))
	f.Add(uint8(0), uint8(3), true, true, make([]byte, 33))
	reserved := reservedSlotTypeBurst(f, enums.DataTypeReserved)
	f.Add(uint8(0), uint8(1), true, false, reserved[:])
	f.Add(uint8(2), uint8(3), false, false, reserved[:])
	f.Fuzz(func(t *testing.T, frameType, dtype uint8, group, slot bool, raw []byte) {
		tr := newTestTranslator(t)
		pkt := makeTestMMDVMPacket(group, slot, uint(frameType%4), uint(dtype))
		copy(pkt.DMRData[:], raw)
		for i := 0; i < 3; i++ {
			tr.TranslateToIPSC(pkt)
		}
	})
}

func TestTranslateRandomInputsDoNotPanic(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input
	tr := newTestTranslator(t)
	for i := 0; i < 20000; i++ {
		n := rng.IntN(130)
		data := make([]byte, n)
		for j := range data {
			data[j] = byte(rng.UintN(256)) //nolint:gosec // bounded
		}
		packetType := []byte{0x80, 0x81, 0x83, 0x84}[rng.IntN(4)]
		if n > 30 && rng.IntN(2) == 0 {
			data[30] = []byte{ipscBurstVoiceHead, ipscBurstVoiceTerm, ipscBurstCSBK, ipscBurstSlot1, ipscBurstSlot2}[rng.IntN(5)]
		}
		tr.TranslateToMMDVM(packetType, data)

		pkt := makeTestMMDVMPacket(rng.IntN(2) == 0, rng.IntN(2) == 0, rng.UintN(4), rng.UintN(16))
		pkt.StreamID = rng.UintN(8)
		for j := range pkt.DMRData {
			pkt.DMRData[j] = byte(rng.UintN(256)) //nolint:gosec // bounded
		}
		tr.TranslateToIPSC(pkt)
	}
}

func TestTranslateToIPSCDataBurstTypes(t *testing.T) {
	t.Parallel()
	want := map[enums.DataType]byte{
		enums.DataTypePIHeader:        0x00,
		enums.DataTypeCSBK:            0x03,
		enums.DataTypeMBCHeader:       0x04,
		enums.DataTypeMBCContinuation: 0x05,
		enums.DataTypeDataHeader:      0x06,
		enums.DataTypeRate12:          0x07,
	}
	for dt, burstType := range want {
		for _, slot := range []bool{false, true} {
			tr := newTestTranslator(t)
			out := tr.TranslateToIPSC(makeTestMMDVMPacket(true, slot, mmdvmFrameTypeDataSync, uint(dt)))
			if len(out) != 1 {
				t.Fatalf("%s: expected 1 packet, got %d", dt.String(), len(out))
			}
			if out[0][30] != burstType {
				t.Fatalf("%s: expected burst type 0x%02X, got 0x%02X", dt.String(), burstType, out[0][30])
			}
		}
	}
}

func TestTranslateToIPSCNeverEmitsVoiceBurstTypeForData(t *testing.T) {
	t.Parallel()
	for dt := enums.DataTypePIHeader; dt <= enums.DataTypeReserved; dt++ {
		if dt == enums.DataTypeVoiceLCHeader || dt == enums.DataTypeTerminatorWithLC {
			continue
		}
		for _, slot := range []bool{false, true} {
			tr := newTestTranslator(t)
			for _, p := range tr.TranslateToIPSC(makeTestMMDVMPacket(true, slot, mmdvmFrameTypeDataSync, uint(dt))) {
				if p[30] == ipscBurstSlot1 || p[30] == ipscBurstSlot2 {
					t.Fatalf("%s: emitted voice burst type 0x%02X", dt.String(), p[30])
				}
			}
		}
	}
}

func TestTranslateToIPSCDropsUnverifiedDataRates(t *testing.T) {
	t.Parallel()
	for _, dt := range []enums.DataType{enums.DataTypeRate34, enums.DataTypeRate1} {
		tr := newTestTranslator(t)
		if out := tr.TranslateToIPSC(makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, uint(dt))); out != nil {
			t.Fatalf("%s: expected nil, got %d packets", dt.String(), len(out))
		}
	}
}

func TestTranslateToMMDVMDataBurstTypes(t *testing.T) {
	t.Parallel()
	for _, burstType := range []byte{0x00, 0x03, 0x04, 0x05, 0x06, 0x07} {
		tr := newTestTranslator(t)
		data := makeTestIPSCPacket(0x83, burstType, true, false)
		out := tr.TranslateToMMDVM(0x83, data)
		if len(out) != 1 {
			t.Fatalf("burst 0x%02X: expected 1 packet, got %d", burstType, len(out))
		}
		if out[0].FrameType != mmdvmFrameTypeDataSync || out[0].DTypeOrVSeq != uint(burstType) {
			t.Fatalf("burst 0x%02X: got frameType %d dtype %d", burstType, out[0].FrameType, out[0].DTypeOrVSeq)
		}
	}
}

func TestTranslateToMMDVMDropsUnsupportedDataBurstTypes(t *testing.T) {
	t.Parallel()
	for _, burstType := range []byte{0x08, 0x09, 0x0B, 0x0C, 0x40} {
		tr := newTestTranslator(t)
		data := makeTestIPSCPacket(0x83, burstType, true, false)
		if out := tr.TranslateToMMDVM(0x83, data); out != nil {
			t.Fatalf("burst 0x%02X: expected nil, got %d packets", burstType, len(out))
		}
	}
}
