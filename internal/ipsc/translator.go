package ipsc

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

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
	colorCode      enums.ColorCode
	streams        map[uint32]*streamState
	reverseStreams map[uint32]*reverseStreamState
	burst          layer2.Burst // reusable burst to reduce allocations
	now            func() time.Time
	lastPrune      time.Time

	nextCallControl uint32
	nextCallSeq     uint8
	nextStreamID    uint32
}

// Stream state that sees no packets for streamIdleTimeout is dropped, so a
// lost terminator or a data-only transaction does not leak. Pruning runs on
// access, at most once per pruneInterval.
const (
	streamIdleTimeout = 5 * time.Second
	pruneInterval     = time.Second
)

// streamState tracks RTP sequencing and call framing for one voice stream.
type streamState struct {
	callControl  uint32 // random per-call
	rtpSeq       uint16
	rtpTimestamp uint32
	ipscSeq      uint8 // byte 5 of every packet of the call
	headersSent  int   // number of voice headers sent (3 required)
	lastSeen     time.Time
	lc           [9]byte
	hasLC        bool // lc holds the full LC from the voice header
	privacy      bool // the call is encrypted: privacy LC or a PI header
	firstPacket  bool // true for the very first packet
}

// IPSC burst type constants (byte 30 of an IPSC user packet). Voice header,
// terminator, and data bursts carry the DMR data type (ETSI TS 102 361-1
// 9.3.6) in this byte; voice bursts use 0x0A (TS1) and 0x8A (TS2). Rate 1
// (0x0A) data is not carried: it would read as a TS1 voice burst, and an
// XPR 8400 does not relay it either. It sends a rate 1 block it received
// cleanly as a 0x06 packet with a meaningless BPTC(196,96) decode, and a
// marginal one as a 0x1F packet with the raw 196 information bits.
const (
	ipscBurstPIHeader        byte = 0x00
	ipscBurstVoiceHead       byte = 0x01
	ipscBurstVoiceTerm       byte = 0x02
	ipscBurstCSBK            byte = 0x03
	ipscBurstMBCHeader       byte = 0x04
	ipscBurstMBCContinuation byte = 0x05
	ipscBurstDataHeader      byte = 0x06
	ipscBurstRate12          byte = 0x07
	ipscBurstRate34          byte = 0x08
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
	ipscMinRate34Len     = 56 // through the 18-byte rate 3/4 payload at 38-55
	ipscMinVoicePktLen   = 52 // through the 19-byte AMBE payload at 33-51
)

// ipscVoiceCtrlSync is the flag in byte 32 of an IPSC voice burst that marks
// a burst carrying voice sync, which is burst A.
const ipscVoiceCtrlSync = 0x40

// ipscCallInfoEnd is the call end flag in byte 17 of an IPSC user packet.
const ipscCallInfoEnd = 0x40

// ipscCallInfoPrivacy is the flag in byte 17 of an IPSC user packet that an
// XPR 8400 sets on every packet of an encrypted voice call.
const ipscCallInfoPrivacy = 0x80

// ipscBurstSyncUndetect is the burst type of the 34-byte placeholder a
// repeater sends for a burst it lost because it lost sync. Bit 7 is the
// timeslot.
const ipscBurstSyncUndetect byte = 0x13

// RTP timestamp increment per burst (~60ms spacing in 16.16 format)
const rtpTimestampIncrement = 480

func NewIPSCTranslator() (*IPSCTranslator, error) {
	return &IPSCTranslator{
		streams:        make(map[uint32]*streamState),
		reverseStreams: make(map[uint32]*reverseStreamState),
		now:            time.Now,
	}, nil
}

// pruneLocked drops stream state in both directions that has been idle for
// longer than streamIdleTimeout. The caller must hold t.mu.
func (t *IPSCTranslator) pruneLocked(now time.Time) {
	if now.Sub(t.lastPrune) < pruneInterval {
		return
	}
	t.lastPrune = now
	for id, ss := range t.streams {
		if now.Sub(ss.lastSeen) > streamIdleTimeout {
			slog.Debug("IPSCTranslator: pruning idle MMDVM stream", "streamID", id)
			t.removeStreamLocked(id)
		}
	}
	for callControl, rss := range t.reverseStreams {
		if now.Sub(rss.lastSeen) > streamIdleTimeout {
			slog.Debug("IPSCTranslator: pruning idle IPSC stream", "callControl", callControl)
			t.removeReverseStreamLocked(callControl)
		}
	}
}

// removeStreamLocked deletes MMDVM-to-IPSC stream state and updates the
// active stream gauge. The caller must hold t.mu.
func (t *IPSCTranslator) removeStreamLocked(streamID uint32) {
	if _, ok := t.streams[streamID]; !ok {
		return
	}
	delete(t.streams, streamID)
	if t.metrics != nil {
		t.metrics.TranslatorActiveStreams.WithLabelValues("mmdvm_to_ipsc").Dec()
	}
}

// removeReverseStreamLocked deletes IPSC-to-MMDVM stream state and updates
// the active stream gauge. The caller must hold t.mu.
func (t *IPSCTranslator) removeReverseStreamLocked(callControl uint32) {
	if _, ok := t.reverseStreams[callControl]; !ok {
		return
	}
	delete(t.reverseStreams, callControl)
	if t.metrics != nil {
		t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Dec()
	}
}

// SetMetrics configures the metrics collector for this translator.
func (t *IPSCTranslator) SetMetrics(m *metrics.Metrics) {
	t.metrics = m
}

// SetColorCode sets the color code of the DMR bursts built for MMDVM: the
// slot type of data bursts and the EMB of voice bursts.
func (t *IPSCTranslator) SetColorCode(colorCode uint8) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.colorCode = enums.ColorCode(colorCode & 0x0F)
}

// SetPeerID sets the local peer ID used in outgoing IPSC packets.
func (t *IPSCTranslator) SetPeerID(peerID uint32) {
	t.peerID = peerID
	t.repeaterID = peerID
}

// TranslateToIPSC converts an MMDVM DMRD Packet into one or more IPSC
// user packets ready to send to IPSC peers. It returns nil if the packet
// cannot be translated (e.g. non-voice data we don't handle yet).
func (t *IPSCTranslator) TranslateToIPSC(pkt mmdvm.Packet) (results [][]byte) {
	t.mu.Lock()
	defer t.mu.Unlock()

	streamID := pkt.StreamID
	if streamID > math.MaxUint32 {
		return nil
	}

	now := t.now()
	t.pruneLocked(now)

	// Get or create stream state
	ss, existed := t.streams[uint32(streamID)]
	if !existed {
		t.nextCallControl++
		if t.nextCallControl == 0 {
			t.nextCallControl = 1
		}
		ss = &streamState{
			callControl: t.nextCallControl,
			ipscSeq:     t.nextCallSeq,
			firstPacket: true,
		}
		t.nextCallSeq++
		t.streams[uint32(streamID)] = ss
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("mmdvm_to_ipsc").Inc()
		}
	}
	ss.lastSeen = now
	defer func(id uint32) {
		if !existed && len(results) == 0 {
			t.removeStreamLocked(id)
		}
	}(uint32(streamID))

	frameType := pkt.FrameType
	dtypeOrVSeq := pkt.DTypeOrVSeq

	switch frameType {
	case mmdvmFrameTypeDataSync:
		if dtypeOrVSeq > 255 {
			slog.Debug("IPSCTranslator: invalid dtype", "dtype", dtypeOrVSeq)
			return nil
		}
		// Voice LC Header, Terminator, or Data
		switch enums.DataType(dtypeOrVSeq) {
		case enums.DataTypeVoiceLCHeader:
			lc := fullLCFromBurst(pkt, enums.DataTypeVoiceLCHeader)
			copy(ss.lc[:], lc[:9])
			ss.hasLC = true
			ss.privacy = ss.privacy || lcPrivacy(ss.lc)
			// Send voice header (IPSC sends 3 copies)
			for i := 0; i < 3; i++ {
				data := t.buildVoiceHeader(pkt, ss, i == 0 && ss.firstPacket, lc)
				results = append(results, data)
			}
			ss.headersSent = 3
			ss.firstPacket = false
		case enums.DataTypeTerminatorWithLC:
			data := t.buildVoiceTerminator(pkt, ss, fullLCFromBurst(pkt, enums.DataTypeTerminatorWithLC))
			results = append(results, data)
			t.removeStreamLocked(uint32(streamID))
		case enums.DataTypeRate34:
			payload, res := layer2.RawBurst(pkt.DMRData).Rate34Payload()
			if res.Uncorrectable {
				slog.Debug("IPSCTranslator: dropping rate 3/4 burst with uncorrectable trellis errors")
				return nil
			}
			results = append(results, t.buildIPSCRate34Packet(pkt, ss, payload))
			ss.firstPacket = false
		case enums.DataTypeCSBK, enums.DataTypePIHeader,
			enums.DataTypeDataHeader, enums.DataTypeRate12,
			enums.DataTypeRate1,
			enums.DataTypeMBCHeader, enums.DataTypeMBCContinuation:
			burstType, ok := ipscDataBurstType(enums.DataType(dtypeOrVSeq))
			if ok && burstType == ipscBurstPIHeader {
				ss.privacy = true
			}
			if !ok {
				slog.Debug("IPSCTranslator: dropping data burst with no verified IPSC encoding",
					"dtype", enums.DataType(dtypeOrVSeq).String())
				return nil
			}
			payload, ok := decodeBPTCPayload(pkt.DMRData, enums.DataType(dtypeOrVSeq))
			if !ok {
				slog.Debug("IPSCTranslator: dropping data burst with uncorrectable BPTC errors",
					"dtype", enums.DataType(dtypeOrVSeq).String())
				return nil
			}
			data := t.buildIPSCDataPacket(pkt, ss, burstType, payload)
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

	default:
		slog.Debug("IPSCTranslator: unknown frame type", "frameType", frameType)
		return nil
	}

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("mmdvm_to_ipsc").Add(float64(len(results)))
	}

	return results
}

// CleanupStream removes state for an MMDVM stream ID in both directions
// (e.g. on timeout): the MMDVM-to-IPSC stream with that ID and any
// IPSC-to-MMDVM stream that was assigned that ID.
func (t *IPSCTranslator) CleanupStream(streamID uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeStreamLocked(streamID)
	for callControl, rss := range t.reverseStreams {
		if rss.streamID == streamID {
			t.removeReverseStreamLocked(callControl)
		}
	}
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

	// Byte 5: call sequence number, the same in every packet of a call and
	// one more for each call, as an XPR 8400 sends it
	buf[5] = ss.ipscSeq

	// Bytes 6-8: Source subscriber (24-bit)
	buf[6] = byte((pkt.Src >> 16) & 0xFF)
	buf[7] = byte((pkt.Src >> 8) & 0xFF)
	buf[8] = byte(pkt.Src & 0xFF)

	// Bytes 9-11: Destination (24-bit)
	buf[9] = byte((pkt.Dst >> 16) & 0xFF)
	buf[10] = byte((pkt.Dst >> 8) & 0xFF)
	buf[11] = byte(pkt.Dst & 0xFF)

	// Byte 12: call priority. MOTOTRBO repeaters send 0x02 on group and
	// private voice, and 0x01 on PVT_DATA.
	buf[12] = 0x02
	if buf[0] == 0x84 {
		buf[12] = 0x01
	}

	// Bytes 13-16: Call control (random per-call)
	binary.BigEndian.PutUint32(buf[13:17], ss.callControl)

	// Byte 17: Call info (privacy, end flag, timeslot)
	callInfo := byte(0x00)
	if ss.privacy {
		callInfo |= ipscCallInfoPrivacy
	}
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
func (t *IPSCTranslator) buildVoiceHeader(pkt mmdvm.Packet, ss *streamState, isFirst bool, lc [12]byte) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, false, false)

	// RTP header: marker on first header, payload type 0x5D
	t.buildRTPHeader(buf, ss, isFirst, 0x5D)

	// RTP Payload — voice header with the full LC
	writeIPSCDataBurst(buf, ipscBurstVoiceHead, 0x00, lc[:], slotTypeColorCode(pkt.DMRData))
	return buf
}

// buildVoiceTerminator builds a 54-byte IPSC voice terminator packet.
func (t *IPSCTranslator) buildVoiceTerminator(pkt mmdvm.Packet, ss *streamState, lc [12]byte) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, true, false)

	// RTP header: no marker, payload type 0x5E for terminator
	t.buildRTPHeader(buf, ss, false, 0x5E)

	// RTP Payload — voice terminator (same structure as header)
	writeIPSCDataBurst(buf, ipscBurstVoiceTerm, 0x00, lc[:], slotTypeColorCode(pkt.DMRData))

	return buf
}

// buildIPSCDataPacket builds a 54-byte IPSC packet for a burst with a
// BPTC(196,96) payload, such as a CSBK or a data header. The structure is
// identical to the voice header and terminator. Data bursts go in data
// packets (0x83/0x84); a PI header belongs to a voice call, and an XPR 8400
// sends it in a voice packet (0x80/0x81).
func (t *IPSCTranslator) buildIPSCDataPacket(pkt mmdvm.Packet, ss *streamState, burstType byte, payload [12]byte) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, false, burstType != ipscBurstPIHeader)

	// RTP header: no marker, payload type 0x5D
	t.buildRTPHeader(buf, ss, ss.firstPacket, 0x5D)

	// RTP Payload — data burst with the BPTC(196,96)-decoded payload
	writeIPSCDataBurst(buf, burstType, 0x40, payload[:], slotTypeColorCode(pkt.DMRData))

	return buf
}

// buildIPSCRate34Packet builds a 60-byte IPSC rate 3/4 data packet. An XPR
// 8400 carries the 18 octets of a rate 3/4 block (ETSI TS 102 361-1 9.2.10)
// rotated left by two: in a confirmed block the DBSN and CRC-9 octets go
// after the 16 data octets, and an unconfirmed block is rotated the same way.
func (t *IPSCTranslator) buildIPSCRate34Packet(pkt mmdvm.Packet, ss *streamState, payload [18]byte) []byte {
	buf := make([]byte, 60)
	t.buildIPSCHeader(buf, pkt, ss, false, true)
	t.buildRTPHeader(buf, ss, ss.firstPacket, 0x5D)
	var rotated [18]byte
	copy(rotated[:], payload[2:])
	copy(rotated[16:], payload[:2])
	writeIPSCDataBurst(buf, ipscBurstRate34, 0x40, rotated[:], slotTypeColorCode(pkt.DMRData))
	return buf
}

// writeIPSCDataBurst writes the RTP payload (bytes 30 to the end of buf) of
// an IPSC voice header, terminator, or data packet, after buildIPSCHeader
// has set the timeslot in byte 17. buf holds payload and 4 more bytes. The
// layout follows MOTOTRBO captures (DMRlink voice_burst_decoding.txt,
// node-dmr-lib's DMRDataPayload, and an XPR 8400 on the test rig):
//
//	30     burst type (the DMR data type)
//	31     bit 7: timeslot 2, the rest: RSSI and parity status flags
//	32-33  length to follow in 16-bit words (bytes 34 to the end)
//	34     0x80: RSSI present
//	35     0x0A: slot type present, data sync
//	36-37  payload size in bits (96, or 144 for rate 3/4)
//	38-    payload: the BPTC(196,96)-decoded octets, or a rate 3/4 block
//	then   slot type: color code (high nibble of the second octet) and
//	       DMR data type
//	then   RSSI (left zero)
func writeIPSCDataBurst(buf []byte, burstType, rssiParity byte, payload []byte, colorCode enums.ColorCode) {
	buf[30] = burstType
	buf[31] = rssiParity&0x7F | (buf[17]&0x20)<<2
	binary.BigEndian.PutUint16(buf[32:34], uint16((len(buf)-34)/2)) //nolint:gosec // buf is at most 60 bytes
	buf[34] = 0x80
	buf[35] = 0x0A
	binary.BigEndian.PutUint16(buf[36:38], uint16(len(payload)*8)) //nolint:gosec // payload is at most 18 bytes
	end := 38 + copy(buf[38:], payload)
	var slotType pdu.SlotTypeInfo
	slotType.SetColorCode(colorCode)
	slotType.SetDataType(enums.DataType(burstType))
	buf[end+1] = byte(slotType)
}

// slotTypeColorCode returns the color code from the slot type of a DMR data
// burst, or 0 when the slot type cannot be decoded.
func slotTypeColorCode(data [33]byte) enums.ColorCode {
	st := layer2.RawBurst(data).SlotType()
	if st.FEC.Uncorrectable {
		return 0
	}
	return st.ColorCode
}

// Superframe positions of the six voice bursts.
const (
	burstA = iota
	burstB
	burstC
	burstD
	burstE
	burstF
)

// mmdvmVoiceBurstPosition returns the superframe position (0-5 for A-F) of
// an MMDVM voice packet. A voice sync frame is always burst A; other voice
// frames carry their position in the voice sequence field.
func mmdvmVoiceBurstPosition(pkt mmdvm.Packet) (int, bool) {
	if pkt.FrameType == mmdvmFrameTypeVoiceSync {
		return burstA, true
	}
	if pkt.DTypeOrVSeq < burstB || pkt.DTypeOrVSeq > burstF {
		return 0, false
	}
	return int(pkt.DTypeOrVSeq), true
}

// defaultLCSS is the embedded LC start/stop value a voice superframe uses at
// each burst position.
func defaultLCSS(pos int) enums.LCSS {
	switch pos {
	case burstB:
		return enums.LCSSFirstFragment
	case burstC, burstD:
		return enums.LCSSContinuationFragment
	case burstE:
		return enums.LCSSLastFragment
	default:
		return enums.LCSSSingleFragment
	}
}

// embByte encodes the IPSC EMB byte (color code, PI, and LCSS, FEC removed)
// that follows the embedded data in bursts B-F. It uses the color code and
// LCSS of the burst's decoded EMB when available, otherwise the LCSS for
// the burst position. PI, the pre-emption and power control indicator, is
// always clear, as in ipsc2hbpc.
func embByte(burst *layer2.Burst, pos int) byte {
	emb := pdu.EmbeddedSignalling{LCSS: defaultLCSS(pos)}
	if burst.HasEmbeddedSignalling() && !burst.EmbeddedSignalling.FEC.Uncorrectable {
		emb = burst.EmbeddedSignalling
	}
	info := emb.Info()
	info.SetPI(false)
	return byte(info)
}

// ipscDataBurstType maps a DMR data type with a 12-byte BPTC(196,96)
// payload to its IPSC burst type byte. It reports false for other data
// types: rate 3/4 has its own layout (buildIPSCRate34Packet), and IPSC
// cannot carry rate 1, whose 0x0A would read as a TS1 voice burst.
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

	burstIdx, ok := mmdvmVoiceBurstPosition(pkt)
	if !ok {
		slog.Debug("IPSCTranslator: invalid voice sequence", "frameType", pkt.FrameType, "vseq", pkt.DTypeOrVSeq)
		return nil
	}
	emb := embByte(&t.burst, burstIdx)

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

		// Bytes 56-64: the call's full LC with the destination and source
		// of the packet
		lc := pdu.LCBytes(ss.lc)
		if !ss.hasLC {
			lc = constructLC(pkt.Src, pkt.Dst, pkt.GroupCall)
		}
		lc.SetAddresses(lcAddress(pkt.Dst), lcAddress(pkt.Src))
		copy(buf[56:65], lc[:])
		buf[65] = emb

	default: // Bursts B, C, D, F — 57 bytes with embedded signalling
		buf = make([]byte, 57)
		t.buildIPSCHeader(buf, pkt, ss, false, false)
		t.buildRTPHeader(buf, ss, false, 0x5D)

		buf[30] = slotBurst
		buf[31] = 0x19 // Length: 25 bytes follow
		buf[32] = 0x06 // Unknown field
		copy(buf[33:52], ambeData[:])

		// Bytes 52-55: embedded signalling data, byte 56: EMB
		if t.burst.HasEmbeddedSignalling() {
			raw := layer2.RawBurst(pkt.DMRData)
			embData := raw.EmbeddedData()
			copy(buf[52:56], embData[:])
		}
		buf[56] = emb
	}

	return buf
}

// decodeBPTCPayload returns the 12-byte BPTC(196,96) payload of a 33-byte
// DMR data burst. The BPTC(196,96) decoder never rejects a burst by itself,
// as in MMDVMHost's CBPTC19696::decode. The payload is accepted as MMDVMHost
// accepts a network burst, by layer2.MMDVMHostAccepts for dataType, the data
// type in the DMRD header. The slot type inside the burst is not used.
func decodeBPTCPayload(data [33]byte, dataType enums.DataType) ([12]byte, bool) {
	raw := layer2.RawBurst(data)
	payload, _ := raw.BPTCPayload()
	return payload, layer2.MMDVMHostAccepts(dataType, payload)
}

// constructFullLC builds a 12-byte voice full LC (9 LC bytes plus masked
// RS(12,9) parity) from call fields. It is the fallback when no valid LC is
// available from the source. dataType is a voice LC header or terminator.
func constructFullLC(src, dst uint, groupCall bool, dataType enums.DataType) [12]byte {
	return fullLCCodeword(constructLC(src, dst, groupCall), dataType)
}

// fullLCCodeword returns the codeword of lc in a burst of dataType: a
// terminator with LC, or else a voice LC header.
func fullLCCodeword(lc pdu.LCBytes, dataType enums.DataType) [12]byte {
	if dataType != enums.DataTypeTerminatorWithLC {
		dataType = enums.DataTypeVoiceLCHeader
	}
	cw, _ := lc.Codeword(dataType) // never fails for a voice LC header or terminator
	return cw
}

// constructLC builds the 9-byte LC of a group or unit-to-unit voice call
// with the standard feature set ID and the default service options.
func constructLC(src, dst uint, groupCall bool) pdu.LCBytes {
	var lc pdu.LCBytes
	lc.SetGroup(groupCall)
	lc.SetFID(enums.FIDStandardized)
	lc.SetServiceOptions(0x20) // the default reserved bit
	lc.SetAddresses(lcAddress(dst), lcAddress(src))
	return lc
}

// lcPrivacy reports whether lc is a group or unit-to-unit voice LC with the
// privacy service option.
func lcPrivacy(lc [9]byte) bool {
	flc, err := pdu.DecodeFullLinkControlBytes(lc)
	switch {
	case err != nil:
		return false
	case flc.GroupVoice != nil:
		return flc.GroupVoice.ServiceOptions.IsPrivacy
	case flc.UnitToUnit != nil:
		return flc.UnitToUnit.ServiceOptions.IsPrivacy
	}
	return false
}

// lcAddress returns the low 24 bits of id, the part of it that the DMRD and
// IPSC headers carry.
func lcAddress(id uint) uint32 {
	return uint32(id & 0xFFFFFF)
}

// mmdvmVoiceDecodeOptions returns the decode options for the burst of an
// MMDVM voice packet: a voice burst at the superframe position that
// mmdvmVoiceBurstPosition gives.
func mmdvmVoiceDecodeOptions(pkt mmdvm.Packet) *layer2.DecodeOptions {
	opts := &layer2.DecodeOptions{Kind: layer2.KindVoice}
	if pos, ok := mmdvmVoiceBurstPosition(pkt); ok {
		opts.VoiceBurst = enums.VoiceBurstA + enums.VoiceBurstType(pos) //nolint:gosec // pos is 0-5
	}
	return opts
}

// fullLCFromBurst returns the full LC for an MMDVM voice LC header or
// terminator burst. The feature set ID and service options come from the LC
// in the burst when it is valid; the call type, destination, and source come
// from the packet fields, which a rewrite rule may have changed.
func fullLCFromBurst(pkt mmdvm.Packet, dataType enums.DataType) [12]byte {
	return fullLCCodeword(rewrittenLC(&pkt, dataType), dataType)
}

// reverseStreamState tracks per-call state for IPSC→MMDVM translation.
type reverseStreamState struct {
	streamID     uint32
	seq          uint8
	anchorPos    int    // superframe position of the last burst A or E
	anchorRTPSeq uint16 // RTP sequence number of that burst
	hasAnchor    bool
	lastRTPSeq   uint16 // RTP sequence number of the last voice burst
	hasLast      bool
	started      bool // whether we've seen a voice header
	lastSeen     time.Time

	// RTP timestamp of the last voice header or burst.
	rtpTimestamp uint32
	hasTimestamp bool

	// Call fields and voice header LC of the last voice packet sent, for
	// the terminator that ends the stream on an RTP discontinuity.
	voiceSent bool
	src, dst  uint
	groupCall bool
	slot      bool
	headerLC  [12]byte
	hasLC     bool
}

// rtpMaxForward is the largest forward RTP timestamp step (8 kHz units,
// 1 second) that still continues a call, as in ipsc2hbpc's
// OUT_RTP_MAX_FWD. A larger step is a new call that reuses the call
// control. Unlike ipsc2hbpc, a backward step this small is a reordered
// packet, not a new call.
const rtpMaxForward = 8000

// TranslateToMMDVM converts raw IPSC user packet data into MMDVM DMRD Packets.
// Returns nil if the packet cannot be translated.
func (t *IPSCTranslator) TranslateToMMDVM(packetType byte, data []byte) (results []mmdvm.Packet) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.pruneLocked(now)

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
	isEnd := (callInfo & ipscCallInfoEnd) != 0

	slog.Debug("IPSCTranslator: TranslateToMMDVM",
		"packetType", fmt.Sprintf("0x%02X", packetType),
		"src", src, "dst", dst, "groupCall", groupCall,
		"slot", slot, "isEnd", isEnd)

	// Determine what kind of IPSC burst this is from byte 30
	burstType := data[30]

	if burstType&0x7F == ipscBurstSyncUndetect {
		if rss, ok := t.reverseStreams[binary.BigEndian.Uint32(data[13:17])]; ok {
			rss.lastSeen = now
			superframeRestartsAfter(rss, binary.BigEndian.Uint16(data[20:22]))
		}
		return nil
	}

	minLen := ipscMinLCPacketLen
	switch burstType {
	case ipscBurstSlot1, ipscBurstSlot2:
		minLen = ipscMinVoicePktLen
	case ipscBurstRate34:
		minLen = ipscMinRate34Len
	}
	if len(data) < minLen {
		slog.Debug("IPSCTranslator: IPSC burst too short",
			"burstType", fmt.Sprintf("0x%02X", burstType), "length", len(data))
		return nil
	}

	// Use call control bytes as stream identifier
	callControl := binary.BigEndian.Uint32(data[13:17])

	// Get or create reverse stream state
	rss, existed := t.reverseStreams[callControl]
	if !existed {
		rss = &reverseStreamState{
			streamID: t.newStreamIDLocked(),
		}
		t.reverseStreams[callControl] = rss
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Inc()
		}
	}
	rss.lastSeen = now
	defer func() {
		if !existed && len(results) == 0 {
			t.removeReverseStreamLocked(callControl)
		}
	}()

	isVoice := burstType == ipscBurstVoiceHead || burstType == ipscBurstSlot1 || burstType == ipscBurstSlot2
	if isVoice {
		results = append(results, t.checkRTPContinuity(rss, binary.BigEndian.Uint32(data[22:26]))...)
	}
	sent := len(results)

	switch burstType {
	case ipscBurstVoiceHead:
		// Voice follows the header with burst A, so the header holds the
		// place of a burst F.
		rss.anchorPos, rss.anchorRTPSeq, rss.hasAnchor = burstF, binary.BigEndian.Uint16(data[20:22]), true
		rss.hasLast = false
		// Voice LC Header — only process the first one (IPSC sends 3)
		if !rss.started {
			pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
				enums.DataTypeVoiceLCHeader, data)
			results = append(results, pkt)
			rss.started = true
		}
		// Skip duplicate headers

	case ipscBurstVoiceTerm:
		// Voice Terminator
		pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
			enums.DataTypeTerminatorWithLC, data)
		results = append(results, pkt)
		t.removeReverseStreamLocked(callControl)

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

	case ipscBurstRate34:
		results = append(results, t.buildMMDVMRate34Packet(src, dst, groupCall, slot, rss, data))

	default:
		slog.Debug("IPSCTranslator: unsupported IPSC burst type", "burstType", fmt.Sprintf("0x%02X", burstType))
	}

	// MOTOTRBO repeaters also set the end bit on voice bursts in the middle
	// of a weak-signal call, so a voice burst never ends the stream. As in
	// ipsc2hbpc, the call ends at its terminator, and pruning ends a call
	// whose terminator is lost.
	if isVoice && len(results) > sent {
		rss.voiceSent = true
		rss.src, rss.dst, rss.groupCall, rss.slot = src, dst, groupCall, slot
	}

	if isEnd && burstType != ipscBurstSlot1 && burstType != ipscBurstSlot2 {
		t.removeReverseStreamLocked(callControl)
	}

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("ipsc_to_mmdvm").Add(float64(len(results)))
	}

	return results
}

// newStreamIDLocked returns the next nonzero MMDVM stream ID. The caller
// must hold t.mu.
func (t *IPSCTranslator) newStreamIDLocked() uint32 {
	t.nextStreamID++
	if t.nextStreamID == 0 {
		t.nextStreamID = 1
	}
	return t.nextStreamID
}

// checkRTPContinuity tracks the RTP timestamp ts of a voice header or burst
// and restarts the stream of rss when ts starts a new call. It returns the
// terminator for the old stream, if any.
func (t *IPSCTranslator) checkRTPContinuity(rss *reverseStreamState, ts uint32) []mmdvm.Packet {
	step := ts - rss.rtpTimestamp
	switch {
	case !rss.hasTimestamp || step <= rtpMaxForward:
		rss.rtpTimestamp, rss.hasTimestamp = ts, true
		return nil
	case -step <= rtpMaxForward:
		return nil
	default:
		slog.Debug("IPSCTranslator: RTP discontinuity, starting a new stream", "streamID", rss.streamID)
		out := t.restartReverseStream(rss)
		rss.rtpTimestamp, rss.hasTimestamp = ts, true
		return out
	}
}

// restartReverseStream ends the MMDVM stream of rss with a terminator, if
// it sent any voice, and resets rss to a new stream. MMDVMHost ignores a
// voice header while a network call is active, so without the terminator
// the new call would not start.
func (t *IPSCTranslator) restartReverseStream(rss *reverseStreamState) []mmdvm.Packet {
	var out []mmdvm.Packet
	if rss.voiceSent {
		lc := constructFullLC(rss.src, rss.dst, rss.groupCall, enums.DataTypeTerminatorWithLC)
		if rss.hasLC {
			lc = rss.headerLC
			_ = layer2.MMDVMHostSeal(enums.DataTypeTerminatorWithLC, &lc) // never fails for a terminator
		}
		dmrData, err := layer2.BuildDataBurst(lc, enums.DataTypeTerminatorWithLC, t.colorCode, layer2.SyncSourceBS)
		if err != nil {
			slog.Debug("IPSCTranslator: cannot build terminator", "error", err)
		}
		out = append(out, mmdvm.Packet{
			Signature:   mmdvm.DMRDSignature,
			Seq:         uint(rss.seq),
			Src:         rss.src,
			Dst:         rss.dst,
			Repeater:    uint(t.repeaterID),
			Slot:        rss.slot,
			GroupCall:   rss.groupCall,
			FrameType:   mmdvmFrameTypeDataSync,
			DTypeOrVSeq: uint(enums.DataTypeTerminatorWithLC),
			StreamID:    uint(rss.streamID),
			DMRData:     dmrData,
		})
	}
	*rss = reverseStreamState{streamID: t.newStreamIDLocked(), lastSeen: rss.lastSeen}
	return out
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

	// Voice LC headers and terminators pass through when their parity is
	// valid, so flags such as emergency, privacy, and the protect flag survive.
	if (dataType == enums.DataTypeVoiceLCHeader || dataType == enums.DataTypeTerminatorWithLC) &&
		!layer2.MMDVMHostAccepts(dataType, lcBytes) {
		if lc, ok := lateEntryLC(lcBytes, src, dst, groupCall); ok {
			lcBytes = fullLCCodeword(lc, dataType)
		} else {
			slog.Debug("IPSCTranslator: invalid full LC from IPSC, constructing from packet fields",
				"dtype", dataType.String())
			lcBytes = constructFullLC(src, dst, groupCall, dataType)
		}
	}
	if dataType == enums.DataTypeVoiceLCHeader {
		rss.headerLC, rss.hasLC = lcBytes, true
	}

	// Build the 33-byte DMR data burst
	dmrData, err := layer2.BuildDataBurst(lcBytes, dataType, t.colorCode, layer2.SyncSourceBS)
	if err != nil {
		slog.Debug("IPSCTranslator: cannot build data burst", "dtype", dataType.String(), "error", err)
	}
	pkt.DMRData = dmrData

	return pkt
}

// lateEntryLC returns the LC of a voice header that a MOTOTRBO repeater
// built from the embedded LC of a call it joined late. Such a header carries
// the 9 LC octets with zero RS(12,9) parity (and 0x10 0x00 in bytes 34-35
// instead of 0x80 0x0A). The LC is used when its call type and addresses
// match the IPSC header, so its feature set ID and service options survive.
func lateEntryLC(payload [12]byte, src, dst uint, groupCall bool) (pdu.LCBytes, bool) {
	if payload[9] != 0 || payload[10] != 0 || payload[11] != 0 {
		return pdu.LCBytes{}, false
	}
	lc := pdu.LCBytes(payload[:9])
	flco := enums.FLCOUnitToUnitVoiceChannelUser
	if groupCall {
		flco = enums.FLCOGroupVoiceChannelUser
	}
	target, source, ok := lc.Addresses()
	if !ok || lc.FLCO() != flco || target != lcAddress(dst) || source != lcAddress(src) {
		return pdu.LCBytes{}, false
	}
	return lc, true
}

// buildMMDVMRate34Packet builds an MMDVM DMRD packet for an IPSC rate 3/4
// data packet, whose 18-octet block is rotated left by two (see
// buildIPSCRate34Packet).
func (t *IPSCTranslator) buildMMDVMRate34Packet(src, dst uint, groupCall, slot bool, rss *reverseStreamState, ipscData []byte) mmdvm.Packet {
	var block [18]byte
	copy(block[:], ipscData[54:56])
	copy(block[2:], ipscData[38:54])
	dmrData, err := layer2.BuildRate34DataBurst(block, t.colorCode, layer2.SyncSourceBS)
	if err != nil {
		slog.Debug("IPSCTranslator: cannot build rate 3/4 burst", "error", err)
	}
	pkt := mmdvm.Packet{
		Signature:   mmdvm.DMRDSignature,
		Seq:         uint(rss.seq),
		Src:         src,
		Dst:         dst,
		Repeater:    uint(t.repeaterID),
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   mmdvmFrameTypeDataSync,
		DTypeOrVSeq: uint(enums.DataTypeRate34),
		StreamID:    uint(rss.streamID),
		DMRData:     dmrData,
	}
	rss.seq++
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

	burstIdx, ok := ipscVoiceBurstPosition(rss, ipscData)
	if !ok {
		slog.Debug("IPSCTranslator: cannot place voice burst in superframe", "length", len(ipscData))
		return nil
	}

	burst := layer2.Burst{Kind: layer2.KindVoice}
	burst.SetAMBEFrames(frames)

	if burstIdx == 0 {
		// Burst A carries the BS-sourced voice SYNC, as the data bursts
		// carry BS-sourced data SYNC: the repeater is a BS, and a duplex
		// MMDVMHost sends BS-sourced SYNC to the network too.
		burst.SyncPattern = enums.SyncBSSourcedVoice
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

	return []mmdvm.Packet{pkt}
}

// ipscVoiceBurstPosition returns the superframe position (0-5 for A-F) of an
// IPSC voice burst. Only bursts A and E can be told from the packet alone:
// A is 52 bytes or carries the voice sync flag, and E is 66 bytes when the
// repeater reassembled the embedded LC. Within a superframe the repeater
// numbers every burst in the RTP sequence, including the ones it lost, so
// every other burst is placed by its RTP sequence distance to the last A or
// E, or to the voice header, which burst A follows. The LCSS in the EMB byte is not used: on
// a weak signal the repeater sends it wrong. A burst that comes before any
// header, A, or E cannot be placed and is dropped, so a late entry starts
// at the next A or E. A repeated burst is dropped. A burst that is not A is
// never placed at A.
func ipscVoiceBurstPosition(rss *reverseStreamState, data []byte) (int, bool) {
	seq := binary.BigEndian.Uint16(data[20:22])
	if rss.hasLast && seq == rss.lastRTPSeq {
		return 0, false
	}
	rss.lastRTPSeq, rss.hasLast = seq, true
	anchor := -1
	switch {
	case len(data) == 52 || data[32]&ipscVoiceCtrlSync != 0:
		anchor = burstA
	case len(data) == 66:
		anchor = burstE
	}
	pos, ok := anchor, anchor >= 0
	if ok {
		rss.anchorPos, rss.anchorRTPSeq, rss.hasAnchor = anchor, seq, true
	} else if rss.hasAnchor {
		delta := seq - rss.anchorRTPSeq
		ok = delta != 0 && delta < 0x8000
		pos = (rss.anchorPos + int(delta%6)) % 6
		if ok && pos == burstA {
			// Only burst A is a 52-byte burst. A repeater that misses burst
			// A right after a voice header or a lost sync does not count it
			// in the RTP sequence, so this burst is B.
			pos = burstB
			rss.anchorPos, rss.anchorRTPSeq = burstB, seq
		}
	}
	if data[17]&ipscCallInfoEnd != 0 {
		superframeRestartsAfter(rss, seq)
	}
	return pos, ok
}

// superframeRestartsAfter places burst A right after RTP sequence number
// seq. A MOTOTRBO repeater that loses the signal sets the end bit on its
// last voice burst or on a "sync not detected" placeholder for a burst it
// lost, and its next burst starts a new superframe at A, since a receiver
// regains sync only on the voice sync of burst A.
func superframeRestartsAfter(rss *reverseStreamState, seq uint16) {
	rss.anchorPos, rss.anchorRTPSeq, rss.hasAnchor = burstF, seq, true
}

// populateEmbeddedSignalling fills in the embedded signalling of voice
// bursts B-F from an IPSC voice burst. MOTOTRBO repeaters put the 32-bit
// embedded fragment in bytes 52-55 and the EMB in the last byte: byte 56 of
// the 57-byte bursts, byte 65 of the 66-byte burst E, whose bytes 56-64
// repeat the decoded LC. The repeater's EMB byte is not used, as on a weak
// signal it is wrong: the LCSS follows the burst position, as every voice
// superframe uses the same LCSS at each position, the color code is our
// own, and PI is clear, as in ipsc2hbpc.
func (t *IPSCTranslator) populateEmbeddedSignalling(raw *layer2.RawBurst, burstIdx int, ipscData []byte) {
	emb := pdu.EmbeddedSignalling{
		ColorCode: t.colorCode,
		LCSS:      defaultLCSS(burstIdx),
	}
	raw.SetEMB(emb)
	if len(ipscData) != 57 && len(ipscData) != 66 {
		return
	}
	raw.SetEmbeddedData([4]byte(ipscData[52:56]))
}
