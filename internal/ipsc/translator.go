package ipsc

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/vocoder"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	mmdvm "github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

// IPSCTranslator converts MMDVM DMRD packets into IPSC user packets.
// It maintains per-stream state (RTP sequence, timestamp, call control)
// and uses the dmrgo library to FEC-decode AMBE voice data from the
// 33-byte DMR burst into the 19-byte IPSC AMBE payload.
//
// It also converts IPSC user packets back into MMDVM DMRD packets for the
// reverse direction.
type IPSCTranslator struct {
	mu             sync.Mutex
	metrics        *metrics.Metrics
	peerID         uint32
	repeaterID     uint32
	streams        map[uint32]*streamState
	reverseStreams map[uint32]*reverseStreamState
	burst          layer2.Burst // reusable burst to reduce allocations

	nextCallControl uint32
	nextStreamID    uint32
}

// streamState tracks RTP sequencing and call framing for one voice stream.
type streamState struct {
	callControl  uint32 // random per-call
	rtpSeq       uint16
	rtpTimestamp uint32
	ipscSeq      uint8
	headersSent  int  // number of voice headers sent (3 required)
	burstIndex   int  // 0-5 → A-F
	firstPacket  bool // true for the very first packet
}

// IPSC burst type constants (byte 30 of an IPSC user packet). Voice header,
// terminator, and data bursts carry the DMR data type (ETSI TS 102 361-1
// 9.3.6) in this byte; voice bursts use 0x0A (TS1) and 0x8A (TS2). Rate 3/4
// (0x08) and rate 1 (0x0A) data are not carried: rate 1 is indistinguishable
// from a TS1 voice burst.
const (
	ipscBurstPIHeader        byte = 0x00
	ipscBurstVoiceHead       byte = 0x01
	ipscBurstVoiceTerm       byte = 0x02
	ipscBurstCSBK            byte = 0x03
	ipscBurstMBCHeader       byte = 0x04
	ipscBurstMBCContinuation byte = 0x05
	ipscBurstDataHeader      byte = 0x06
	ipscBurstRate12          byte = 0x07
	ipscBurstSlot1           byte = 0x0A
	ipscBurstSlot2           byte = 0x8A
)

// MMDVM FrameType values (bits 2-3 of DMRD byte 15)
const (
	mmdvmFrameTypeVoice     uint = 0 // Voice data
	mmdvmFrameTypeVoiceSync uint = 1 // Voice sync (marks A burst or data sync)
	mmdvmFrameTypeDataSync  uint = 2 // Data sync (header / terminator)
)

// IPSC user packet layout limits.
const (
	ipscMinUserPacketLen = 31 // through the burst type byte at offset 30
	ipscMinLCPacketLen   = 50 // through the 12-byte LC/data payload at 38-49
	ipscMinVoicePktLen   = 52 // through the 19-byte AMBE payload at 33-51
)

// RTP timestamp increment per burst (~60ms spacing in 16.16 format)
const rtpTimestampIncrement = 480

func NewIPSCTranslator() (*IPSCTranslator, error) {
	return &IPSCTranslator{
		streams:        make(map[uint32]*streamState),
		reverseStreams: make(map[uint32]*reverseStreamState),
	}, nil
}

// SetMetrics configures the metrics collector for this translator.
func (t *IPSCTranslator) SetMetrics(m *metrics.Metrics) {
	t.metrics = m
}

// SetPeerID sets the local peer ID used in outgoing IPSC packets.
func (t *IPSCTranslator) SetPeerID(peerID uint32) {
	t.peerID = peerID
	t.repeaterID = peerID
}

// TranslateToIPSC converts an MMDVM DMRD Packet into one or more IPSC
// user packets ready to send to IPSC peers. It returns nil if the packet
// cannot be translated (e.g. non-voice data we don't handle yet).
func (t *IPSCTranslator) TranslateToIPSC(pkt mmdvm.Packet) [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	streamID := pkt.StreamID
	if streamID > math.MaxUint32 {
		return nil
	}

	// Get or create stream state
	ss, ok := t.streams[uint32(streamID)]
	if !ok {
		t.nextCallControl++
		if t.nextCallControl == 0 {
			t.nextCallControl = 1
		}
		ss = &streamState{
			callControl: t.nextCallControl,
			firstPacket: true,
		}
		t.streams[uint32(streamID)] = ss
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("mmdvm_to_ipsc").Inc()
		}
	}

	frameType := pkt.FrameType
	dtypeOrVSeq := pkt.DTypeOrVSeq

	var results [][]byte

	switch frameType {
	case mmdvmFrameTypeDataSync:
		if dtypeOrVSeq > 255 {
			slog.Debug("IPSCTranslator: invalid dtype", "dtype", dtypeOrVSeq)
			return nil
		}
		// Voice LC Header, Terminator, or Data
		switch enums.DataType(dtypeOrVSeq) {
		case enums.DataTypeVoiceLCHeader:
			// Send voice header (IPSC sends 3 copies)
			for i := 0; i < 3; i++ {
				data := t.buildVoiceHeader(pkt, ss, i == 0 && ss.firstPacket)
				results = append(results, data)
			}
			ss.headersSent = 3
			ss.firstPacket = false
			ss.burstIndex = 0
		case enums.DataTypeTerminatorWithLC:
			data := t.buildVoiceTerminator(pkt, ss)
			results = append(results, data)
			// Clean up stream state
			delete(t.streams, uint32(streamID))
			if t.metrics != nil {
				t.metrics.TranslatorActiveStreams.WithLabelValues("mmdvm_to_ipsc").Dec()
			}
		case enums.DataTypeCSBK, enums.DataTypePIHeader,
			enums.DataTypeDataHeader, enums.DataTypeRate12,
			enums.DataTypeRate34, enums.DataTypeRate1,
			enums.DataTypeMBCHeader, enums.DataTypeMBCContinuation:
			burstType, ok := ipscDataBurstType(enums.DataType(dtypeOrVSeq))
			if !ok {
				slog.Debug("IPSCTranslator: dropping data burst with no verified IPSC encoding",
					"dtype", enums.DataType(dtypeOrVSeq).String())
				return nil
			}
			data := t.buildIPSCDataPacket(pkt, ss, burstType)
			results = append(results, data)
			ss.firstPacket = false
		case enums.DataTypeIdle, enums.DataTypeUnifiedSingleBlock, enums.DataTypeReserved:
			return nil
		default:
			slog.Debug("IPSCTranslator: unhandled data sync dtype", "dtype", dtypeOrVSeq)
			return nil
		}

	case mmdvmFrameTypeVoice, mmdvmFrameTypeVoiceSync:
		// Voice burst — decode DMR data and extract AMBE
		data := t.buildVoiceBurst(pkt, ss)
		if data != nil {
			results = append(results, data)
		}
		// Advance burst index (A=0 through F=5, then wrap)
		ss.burstIndex = (ss.burstIndex + 1) % 6

	default:
		slog.Debug("IPSCTranslator: unknown frame type", "frameType", frameType)
		return nil
	}

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("mmdvm_to_ipsc").Add(float64(len(results)))
	}

	return results
}

// CleanupStream removes state for a given stream (e.g. on timeout).
func (t *IPSCTranslator) CleanupStream(streamID uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streams, streamID)
}

// buildIPSCHeader writes the common 18-byte IPSC header (bytes 0-17).
func (t *IPSCTranslator) buildIPSCHeader(buf []byte, pkt mmdvm.Packet, ss *streamState, isEnd bool, isData bool) {
	// Byte 0: Packet type
	if isData {
		if pkt.GroupCall {
			buf[0] = byte(0x83) // GROUP_DATA
		} else {
			buf[0] = byte(0x84) // PVT_DATA
		}
	} else {
		if pkt.GroupCall {
			buf[0] = byte(0x80) // GROUP_VOICE
		} else {
			buf[0] = byte(0x81) // PVT_VOICE
		}
	}

	// Bytes 1-4: Peer ID
	binary.BigEndian.PutUint32(buf[1:5], t.peerID)

	// Byte 5: IPSC sequence number
	buf[5] = ss.ipscSeq

	// Bytes 6-8: Source subscriber (24-bit)
	buf[6] = byte((pkt.Src >> 16) & 0xFF)
	buf[7] = byte((pkt.Src >> 8) & 0xFF)
	buf[8] = byte(pkt.Src & 0xFF)

	// Bytes 9-11: Destination (24-bit)
	buf[9] = byte((pkt.Dst >> 16) & 0xFF)
	buf[10] = byte((pkt.Dst >> 8) & 0xFF)
	buf[11] = byte(pkt.Dst & 0xFF)

	// Byte 12: Call type (0x02 = group call)
	if pkt.GroupCall {
		buf[12] = 0x02
	} else {
		buf[12] = 0x01
	}

	// Bytes 13-16: Call control (random per-call)
	binary.BigEndian.PutUint32(buf[13:17], ss.callControl)

	// Byte 17: Call info (timeslot + end flag)
	callInfo := byte(0x00)
	if pkt.Slot { // true = TS2
		callInfo |= 0x20
	}
	if isEnd {
		callInfo |= 0x40
	}
	buf[17] = callInfo
}

// buildRTPHeader writes the 12-byte RTP header at buf[18:30].
func (t *IPSCTranslator) buildRTPHeader(buf []byte, ss *streamState, marker bool, payloadType byte) {
	// Byte 18: RTP version 2, no padding, no extension, 0 CSRCs
	buf[18] = 0x80

	// Byte 19: Marker + payload type
	pt := payloadType
	if marker {
		pt |= 0x80
	}
	buf[19] = pt

	// Bytes 20-21: RTP sequence number
	binary.BigEndian.PutUint16(buf[20:22], ss.rtpSeq)
	ss.rtpSeq++

	// Bytes 22-25: RTP timestamp
	binary.BigEndian.PutUint32(buf[22:26], ss.rtpTimestamp)
	ss.rtpTimestamp += rtpTimestampIncrement

	// Bytes 26-29: RTP SSRC (0)
	binary.BigEndian.PutUint32(buf[26:30], 0)
}

// buildVoiceHeader builds a 54-byte IPSC voice header packet.
// Voice headers embed the Full LC (link control) data.
func (t *IPSCTranslator) buildVoiceHeader(pkt mmdvm.Packet, ss *streamState, isFirst bool) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, false, false)

	// RTP header: marker on first header, payload type 0x5D
	t.buildRTPHeader(buf, ss, isFirst, 0x5D)

	// RTP Payload — voice header
	burstType := ipscBurstSlot2
	if !pkt.Slot {
		burstType = ipscBurstSlot1
	}
	_ = burstType

	buf[30] = ipscBurstVoiceHead                   // Burst type
	buf[31] = 0x80                                 // RSSI threshold / parity
	binary.BigEndian.PutUint16(buf[32:34], 0x000A) // Length to follow (10 words = 20 bytes)
	buf[34] = 0x80                                 // RSSI status
	if pkt.Slot {
		buf[35] = ipscBurstSlot2 // Slot type/sync
	} else {
		buf[35] = ipscBurstSlot1
	}
	binary.BigEndian.PutUint16(buf[36:38], 0x0060) // Data size (96 bits = 12 bytes)

	// Bytes 38-49: Full LC data (12 bytes)
	// Extract from the DMR burst data — the header burst carries a Voice LC Header
	// which contains FLCO, FID, ServiceOpt, Dst, Src, CRC
	flcBytes := extractFullLCBytes(pkt)
	copy(buf[38:50], flcBytes[:12])

	// Bytes 50-53: unknown trailing (zeros)
	return buf
}

// buildVoiceTerminator builds a 54-byte IPSC voice terminator packet.
func (t *IPSCTranslator) buildVoiceTerminator(pkt mmdvm.Packet, ss *streamState) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, true, false)

	// RTP header: no marker, payload type 0x5E for terminator
	t.buildRTPHeader(buf, ss, false, 0x5E)

	// RTP Payload — voice terminator (same structure as header)
	buf[30] = ipscBurstVoiceTerm
	buf[31] = 0x80
	binary.BigEndian.PutUint16(buf[32:34], 0x000A)
	buf[34] = 0x80
	if pkt.Slot {
		buf[35] = ipscBurstSlot2
	} else {
		buf[35] = ipscBurstSlot1
	}
	binary.BigEndian.PutUint16(buf[36:38], 0x0060)

	// Full LC data
	flcBytes := extractFullLCBytes(pkt)
	copy(buf[38:50], flcBytes[:12])

	ss.ipscSeq++
	return buf
}

// buildIPSCDataPacket builds a 54-byte IPSC data packet for CSBK, Data Header, etc.
// The structure is identical to voice header/terminator but with data packet types (0x83/0x84).
func (t *IPSCTranslator) buildIPSCDataPacket(pkt mmdvm.Packet, ss *streamState, burstType byte) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, false, true)

	// RTP header: no marker, payload type 0x5D
	t.buildRTPHeader(buf, ss, ss.firstPacket, 0x5D)

	// RTP Payload — data burst
	buf[30] = burstType
	buf[31] = 0xC0 // RSSI threshold / parity
	binary.BigEndian.PutUint16(buf[32:34], 0x000A)
	buf[34] = 0x80 // RSSI status
	if pkt.Slot {
		buf[35] = ipscBurstSlot2 // Slot type/sync
	} else {
		buf[35] = ipscBurstSlot1
	}
	binary.BigEndian.PutUint16(buf[36:38], 0x0060) // Data size (96 bits = 12 bytes)

	// Bytes 38-49: Extract data from DMR burst via BPTC decode
	// Use extractFullLCBytes which constructs from packet fields
	flcBytes := extractFullLCBytes(pkt)
	copy(buf[38:50], flcBytes[:12])

	// Bytes 50-53: trailing (zeros)
	ss.ipscSeq++
	return buf
}

// ipscDataBurstType maps a DMR data type to its IPSC burst type byte. It
// reports false for data types that IPSC cannot carry: rate 3/4 and rate 1
// payloads do not fit the verified 12-byte layout, and rate 1 (0x0A) would
// read as a TS1 voice burst.
func ipscDataBurstType(dataType enums.DataType) (byte, bool) {
	switch dataType {
	case enums.DataTypePIHeader:
		return ipscBurstPIHeader, true
	case enums.DataTypeCSBK:
		return ipscBurstCSBK, true
	case enums.DataTypeMBCHeader:
		return ipscBurstMBCHeader, true
	case enums.DataTypeMBCContinuation:
		return ipscBurstMBCContinuation, true
	case enums.DataTypeDataHeader:
		return ipscBurstDataHeader, true
	case enums.DataTypeRate12:
		return ipscBurstRate12, true
	case enums.DataTypeVoiceLCHeader, enums.DataTypeTerminatorWithLC,
		enums.DataTypeRate34, enums.DataTypeIdle, enums.DataTypeRate1,
		enums.DataTypeUnifiedSingleBlock, enums.DataTypeReserved:
		return 0, false
	default:
		return 0, false
	}
}

// buildVoiceBurst builds an IPSC voice burst packet.
// Burst A = 52 bytes, Bursts B-D,F = 57 bytes, Burst E = 66 bytes.
func (t *IPSCTranslator) buildVoiceBurst(pkt mmdvm.Packet, ss *streamState) []byte {
	// Decode the DMR burst to extract AMBE voice data. The DMRD frame type
	// says the burst is voice, so its SYNC field does not classify it.
	_ = t.burst.DecodeFromBytesWith(pkt.DMRData, mmdvmVoiceDecodeOptions(pkt))

	// Extract the 19-byte FEC-decoded AMBE payload from the 3 vocoder frames
	frames, _ := t.burst.AMBEFrames()
	ambeData := vocoder.PackAMBEVoice(frames)

	// Determine slot type byte
	slotBurst := ipscBurstSlot2
	if !pkt.Slot {
		slotBurst = ipscBurstSlot1
	}

	burstIdx := ss.burstIndex % 6

	var buf []byte
	switch burstIdx {
	case 0: // Burst A — sync burst, 52 bytes
		buf = make([]byte, 52)
		t.buildIPSCHeader(buf, pkt, ss, false, false)
		t.buildRTPHeader(buf, ss, false, 0x5D)

		buf[30] = slotBurst
		buf[31] = 0x14 // Length: 20 bytes follow
		buf[32] = 0x40 // Unknown field
		copy(buf[33:52], ambeData[:])

	case 4: // Burst E — extended with embedded LC, 66 bytes
		buf = make([]byte, 66)
		t.buildIPSCHeader(buf, pkt, ss, false, false)
		t.buildRTPHeader(buf, ss, false, 0x5D)

		buf[30] = slotBurst
		buf[31] = 0x22 // Length: 34 bytes follow
		buf[32] = 0x16 // Unknown field
		copy(buf[33:52], ambeData[:])

		// Bytes 52-58: Embedded LC data (7 bytes)
		// Extract from embedded signalling if available
		if t.burst.HasEmbeddedSignalling() {
			raw := layer2.RawBurst(pkt.DMRData)
			embData := raw.EmbeddedData()
			copy(buf[52:56], embData[:])
		}

		// Bytes 56-58 or 59-61: Destination repeated
		buf[59] = byte((pkt.Dst >> 16) & 0xFF)
		buf[60] = byte((pkt.Dst >> 8) & 0xFF)
		buf[61] = byte(pkt.Dst & 0xFF)
		// Bytes 62-64: Source repeated
		buf[62] = byte((pkt.Src >> 16) & 0xFF)
		buf[63] = byte((pkt.Src >> 8) & 0xFF)
		buf[64] = byte(pkt.Src & 0xFF)
		buf[65] = 0x14 // Unknown trailer

	default: // Bursts B, C, D, F — 57 bytes with embedded signalling
		buf = make([]byte, 57)
		t.buildIPSCHeader(buf, pkt, ss, false, false)
		t.buildRTPHeader(buf, ss, false, 0x5D)

		buf[30] = slotBurst
		buf[31] = 0x19 // Length: 25 bytes follow
		buf[32] = 0x06 // Unknown field
		copy(buf[33:52], ambeData[:])

		// Bytes 52-56: Embedded signalling data (5 bytes)
		if t.burst.HasEmbeddedSignalling() {
			raw := layer2.RawBurst(pkt.DMRData)
			embData := raw.EmbeddedData()
			copy(buf[52:56], embData[:])
		}
	}

	return buf
}

// extractFullLCBytes builds 12 bytes of Full Link Control data from the
// packet fields: the LC octets and their masked RS(12,9) parity.
func extractFullLCBytes(pkt mmdvm.Packet) [12]byte {
	var lc pdu.LCBytes
	lc.SetGroup(pkt.GroupCall)
	lc.SetFID(enums.FIDStandardized)
	lc.SetServiceOptions(0x20) // the default reserved bit
	lc.SetAddresses(lcAddress(pkt.Dst), lcAddress(pkt.Src))
	cw, _ := lc.Codeword(enums.DataTypeVoiceLCHeader) // never fails for a voice LC header
	return cw
}

// lcAddress returns the low 24 bits of id, the part of it that the DMRD and
// IPSC headers carry.
func lcAddress(id uint) uint32 {
	return uint32(id & 0xFFFFFF)
}

// mmdvmVoiceDecodeOptions returns the decode options for the burst of an
// MMDVM voice packet: a voice burst at the superframe position that the
// frame type and voice sequence give.
func mmdvmVoiceDecodeOptions(pkt mmdvm.Packet) *layer2.DecodeOptions {
	opts := &layer2.DecodeOptions{Kind: layer2.KindVoice}
	switch {
	case pkt.FrameType == mmdvmFrameTypeVoiceSync:
		opts.VoiceBurst = enums.VoiceBurstA
	case pkt.DTypeOrVSeq >= 1 && pkt.DTypeOrVSeq <= 5:
		opts.VoiceBurst = enums.VoiceBurstA + enums.VoiceBurstType(pkt.DTypeOrVSeq)
	}
	return opts
}

// reverseStreamState tracks per-call state for IPSC→MMDVM translation.
type reverseStreamState struct {
	streamID   uint32
	seq        uint8
	burstIndex int  // 0-5 → A-F within a superframe
	started    bool // whether we've seen a voice header
}

// TranslateToMMDVM converts raw IPSC user packet data into MMDVM DMRD Packets.
// Returns nil if the packet cannot be translated.
func (t *IPSCTranslator) TranslateToMMDVM(packetType byte, data []byte) []mmdvm.Packet {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(data) < ipscMinUserPacketLen {
		slog.Debug("IPSCTranslator: IPSC packet too short", "length", len(data))
		return nil
	}

	// Handle voice (0x80/0x81) and data (0x83/0x84) packet types
	switch packetType {
	case 0x80, 0x81, 0x83, 0x84:
		// OK — supported packet types
	default:
		slog.Debug("IPSCTranslator: ignoring unsupported IPSC packet", "type", packetType)
		return nil
	}

	// Parse the IPSC header
	src := uint(data[6])<<16 | uint(data[7])<<8 | uint(data[8])
	dst := uint(data[9])<<16 | uint(data[10])<<8 | uint(data[11])
	groupCall := packetType == 0x80 || packetType == 0x83
	callInfo := data[17]
	slot := (callInfo & 0x20) != 0 // true = TS2
	isEnd := (callInfo & 0x40) != 0

	slog.Debug("IPSCTranslator: TranslateToMMDVM",
		"packetType", fmt.Sprintf("0x%02X", packetType),
		"src", src, "dst", dst, "groupCall", groupCall,
		"slot", slot, "isEnd", isEnd)

	// Use call control bytes as stream identifier
	callControl := binary.BigEndian.Uint32(data[13:17])

	// Get or create reverse stream state
	rss, ok := t.reverseStreams[callControl]
	if !ok {
		t.nextStreamID++
		if t.nextStreamID == 0 {
			t.nextStreamID = 1
		}
		rss = &reverseStreamState{
			streamID: t.nextStreamID,
		}
		t.reverseStreams[callControl] = rss
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Inc()
		}
	}

	// Determine what kind of IPSC burst this is from byte 30
	burstType := data[30]

	var results []mmdvm.Packet

	minLen := ipscMinLCPacketLen
	if burstType == ipscBurstSlot1 || burstType == ipscBurstSlot2 {
		minLen = ipscMinVoicePktLen
	}
	if len(data) < minLen {
		slog.Debug("IPSCTranslator: IPSC burst too short",
			"burstType", fmt.Sprintf("0x%02X", burstType), "length", len(data))
		return nil
	}

	switch burstType {
	case ipscBurstVoiceHead:
		// Voice LC Header — only process the first one (IPSC sends 3)
		if !rss.started {
			pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
				enums.DataTypeVoiceLCHeader, data)
			results = append(results, pkt)
			rss.started = true
			rss.burstIndex = 0
		}
		// Skip duplicate headers

	case ipscBurstVoiceTerm:
		// Voice Terminator
		pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
			enums.DataTypeTerminatorWithLC, data)
		results = append(results, pkt)
		// Clean up
		delete(t.reverseStreams, callControl)
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Dec()
		}

	case ipscBurstSlot1, ipscBurstSlot2:
		// Voice burst — extract AMBE, FEC-encode, build DMR burst
		pkts := t.buildMMDVMVoiceBurst(src, dst, groupCall, slot, rss, data)
		results = append(results, pkts...)

	case ipscBurstPIHeader, ipscBurstCSBK, ipscBurstMBCHeader,
		ipscBurstMBCContinuation, ipscBurstDataHeader, ipscBurstRate12:
		// Data bursts carry the DMR data type in the burst type byte and the
		// 12-byte BPTC(196,96) payload at the same offset as the voice LC.
		pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
			enums.DataType(burstType), data)
		results = append(results, pkt)

	default:
		slog.Debug("IPSCTranslator: unsupported IPSC burst type", "burstType", fmt.Sprintf("0x%02X", burstType))
		return nil
	}

	if isEnd && burstType != ipscBurstVoiceTerm {
		// End flag set but not a terminator — clean up anyway
		delete(t.reverseStreams, callControl)
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Dec()
		}
	}

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("ipsc_to_mmdvm").Add(float64(len(results)))
	}

	return results
}

// buildMMDVMDataPacket builds an MMDVM DMRD packet for a voice LC header, terminator,
// or data burst (CSBK, Data Header, etc.).
// It constructs the 33-byte DMR burst from the IPSC payload data using BPTC encoding.
func (t *IPSCTranslator) buildMMDVMDataPacket(
	src, dst uint, groupCall, slot bool,
	rss *reverseStreamState,
	dataType enums.DataType,
	ipscData []byte,
) mmdvm.Packet {
	pkt := mmdvm.Packet{
		Signature:   mmdvm.DMRDSignature,
		Seq:         uint(rss.seq),
		Src:         src,
		Dst:         dst,
		Repeater:    uint(t.repeaterID),
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   mmdvmFrameTypeDataSync,
		DTypeOrVSeq: uint(dataType),
		StreamID:    uint(rss.streamID),
	}
	rss.seq++

	// Extract payload bytes from IPSC packet (bytes 38-49 = 12 bytes)
	var lcBytes [12]byte
	copy(lcBytes[:], ipscData[38:50])

	// For voice LC headers and terminators, override the FLCO byte to match
	// the group/private flag from the IPSC packet type.
	if dataType == enums.DataTypeVoiceLCHeader || dataType == enums.DataTypeTerminatorWithLC {
		if groupCall {
			lcBytes[0] = byte(enums.FLCOGroupVoiceChannelUser)
		} else {
			lcBytes[0] = byte(enums.FLCOUnitToUnitVoiceChannelUser)
		}
	}
	// For CSBK/data types, preserve the payload bytes as-is from the radio

	// Build the 33-byte DMR data burst
	dmrData, err := layer2.BuildDataBurst(lcBytes, dataType, 0, layer2.SyncSourceBS)
	if err != nil {
		slog.Debug("IPSCTranslator: cannot build data burst", "dtype", dataType.String(), "error", err)
	}
	pkt.DMRData = dmrData

	return pkt
}

// buildMMDVMVoiceBurst builds MMDVM DMRD packets from an IPSC voice burst.
// It extracts the 19-byte AMBE payload, FEC-encodes back to DMR format,
// and reconstructs the full 33-byte DMR burst with proper sync/EMB.
func (t *IPSCTranslator) buildMMDVMVoiceBurst(
	src, dst uint, groupCall, slot bool,
	rss *reverseStreamState,
	ipscData []byte,
) []mmdvm.Packet {
	// Extract the 19-byte AMBE data from IPSC packet (bytes 33-51)
	var ambeBytes [19]byte
	copy(ambeBytes[:], ipscData[33:52])

	// Unpack into 3 VocoderFrames (49 bits each)
	frames := vocoder.UnpackAMBEVoice(ambeBytes)

	// Determine if this is a sync burst (A) or embedded signalling burst (B-F)
	burstIdx := rss.burstIndex % 6

	burst := layer2.Burst{Kind: layer2.KindVoice}
	burst.SetAMBEFrames(frames)

	if burstIdx == 0 {
		// Burst A — voice sync burst
		burst.SyncPattern = enums.SyncMSSourcedVoice
		burst.VoiceBurst = enums.VoiceBurstA
	} else {
		// Bursts B-F — embedded signalling
		burst.SyncPattern = enums.SyncNone

		switch burstIdx {
		case 1:
			burst.VoiceBurst = enums.VoiceBurstB
		case 2:
			burst.VoiceBurst = enums.VoiceBurstC
		case 3:
			burst.VoiceBurst = enums.VoiceBurstD
		case 4:
			burst.VoiceBurst = enums.VoiceBurstE
		case 5:
			burst.VoiceBurst = enums.VoiceBurstF
		}
	}

	// Encode the burst to 33 bytes. SetAMBEFrames FEC-encoded the 3×49 bit
	// vocoder frames to 3×72 = 216 bits.
	dmrData, err := burst.Encode()
	if err != nil {
		slog.Debug("IPSCTranslator: cannot encode voice burst", "error", err)
		return nil
	}
	if burstIdx != 0 {
		// Extract embedded signalling from the IPSC packet if available
		t.populateEmbeddedSignalling(&dmrData, burstIdx, ipscData)
	}

	// Determine frame type
	if burstIdx < 0 {
		burstIdx = 0
	}

	frameType := mmdvmFrameTypeVoice
	if burstIdx == 0 {
		frameType = mmdvmFrameTypeVoiceSync
	}

	pkt := mmdvm.Packet{
		Signature:   mmdvm.DMRDSignature,
		Seq:         uint(rss.seq),
		Src:         src,
		Dst:         dst,
		Repeater:    uint(t.repeaterID),
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   frameType,
		DTypeOrVSeq: uint(burstIdx), //nolint:gosec // Bounds checked
		StreamID:    uint(rss.streamID),
		DMRData:     dmrData,
	}
	rss.seq++
	rss.burstIndex = (rss.burstIndex + 1) % 6

	return []mmdvm.Packet{pkt}
}

// populateEmbeddedSignalling fills in the embedded signalling fields
// for voice bursts B-F from the IPSC packet's trailing data.
func (t *IPSCTranslator) populateEmbeddedSignalling(raw *layer2.RawBurst, burstIdx int, ipscData []byte) {
	// Set up a basic embedded signalling with color code 0
	emb := pdu.EmbeddedSignalling{
		ColorCode:                          0,
		PreemptionAndPowerControlIndicator: false,
		LCSS:                               enums.LCSSContinuationFragment,
	}

	// Set LCSS based on burst position
	switch burstIdx {
	case 1: // Burst B — first fragment
		emb.LCSS = enums.LCSSFirstFragment
	case 4: // Burst E — last fragment
		emb.LCSS = enums.LCSSLastFragment
	default: // Bursts C, D, F — continuation
		emb.LCSS = enums.LCSSContinuationFragment
	}
	raw.SetEMB(emb)

	// Copy the embedded data from bytes 52-55 if present: bursts B, C, D,
	// and F are 57 bytes and burst E is 66 bytes
	switch len(ipscData) {
	case 57, 66:
		raw.SetEmbeddedData([4]byte(ipscData[52:56]))
	default:
		// No embedded data available
	}
}
