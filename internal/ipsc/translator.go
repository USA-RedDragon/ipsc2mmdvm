package ipsc

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/packet"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/layer3"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/dmrgo/v2/vocoder"
	"github.com/USA-RedDragon/dmrgo/v2/voice"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/logging"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
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
	burst          layer2.Burst  // reusable burst to reduce allocations
	scratch        *layer2.Burst // the decode burst of the voice receivers
	events         []voice.Event // the events of a voice receiver
	logs           logging.Queue // records made while mu is held
	now            func() time.Time
	lastPrune      time.Time

	// endedData holds when each IPSC data call that ended with the end
	// flag ended, by call control, until pruning drops it.
	endedData map[uint32]time.Time

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
	hasLC        bool            // lc holds the full LC from the voice header
	privacy      bool            // the call is encrypted: privacy LC or a PI header
	data         DataCall        // the data packet of the call so far
	voice        *voice.Receiver // learns the LC of a late-entry call from its embedded LC
	firstPacket  bool            // true for the very first packet
	last         dmrd.Data       // the last MMDVM packet of the stream
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
	ipscBurstIdle            byte = 0x09
	ipscBurstSlot1           byte = 0x0A
	ipscBurstUSBD            byte = 0x0B
	ipscBurstRaw             byte = byte(ipscpkt.BurstRaw)
	ipscBurstSlot2           byte = 0x8A
)

// ipscVoiceCtrlSync is the flag in the control octet of an IPSC voice burst
// that marks a burst carrying voice sync, which is burst A.
const ipscVoiceCtrlSync = 0x40

// ipscFlagSlotType is the flag in the second flags octet of an IPSC data
// burst that says a slot type follows the payload.
const ipscFlagSlotType = 0x08

// ipscVoiceCtrlBadFrame1 is the flag in byte 32 of an IPSC voice burst that
// marks its first AMBE frame as uncorrectable on RF receive. An XPR 8400 set
// it only on frames that differed from what was sent. Bits 49 and 99 of the
// AMBE payload flag the second and third frames the same way
// (vocoder.UnpackAMBEVoiceFlags). A flagged frame is replaced with silence,
// as ipsc2hbpc does.
const ipscVoiceCtrlBadFrame1 = 0x01

// The bits of the call info octet of an IPSC user packet: the end flag, the
// timeslot, and the privacy flag that an XPR 8400 sets on every packet of
// an encrypted voice call. ipscpkt.CallInfo reads them but has no setters.
const (
	ipscCallInfoEnd     = 0x40
	ipscCallInfoSlot2   = 0x20
	ipscCallInfoPrivacy = 0x80
)

// ipscStatusRSSI is the RSSI status bit of the status octet of an IPSC data
// burst (see buildDataBurst).
const ipscStatusRSSI = 0x40

// ipscBurstSyncUndetect is the burst type of the 34-byte placeholder a
// repeater sends for a burst it lost because it lost sync. Bit 7 is the
// timeslot.
const ipscBurstSyncUndetect = byte(ipscpkt.BurstSyncLost)

// RTP timestamp increment per burst (~60ms spacing in 16.16 format)
const rtpTimestampIncrement = 480

func NewIPSCTranslator() (*IPSCTranslator, error) {
	return &IPSCTranslator{
		streams:        make(map[uint32]*streamState),
		reverseStreams: make(map[uint32]*reverseStreamState),
		endedData:      make(map[uint32]time.Time),
		scratch:        layer2.NewBurst(),
		events:         make([]voice.Event, 0, voice.MaxEvents),
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
			t.logs.Debug("IPSCTranslator: pruning idle MMDVM stream", "streamID", id)
			t.removeStreamLocked(id)
		}
	}
	for callControl, rss := range t.reverseStreams {
		if now.Sub(rss.lastSeen) > streamIdleTimeout {
			t.logs.Debug("IPSCTranslator: pruning idle IPSC stream", "callControl", callControl)
			t.removeReverseStreamLocked(callControl)
		}
	}
	for callControl, at := range t.endedData {
		if now.Sub(at) > streamIdleTimeout {
			delete(t.endedData, callControl)
		}
	}
}

// endReverseDataCallLocked removes the IPSC-to-MMDVM stream of the data call
// callControl, which ended with the end flag at now. The caller must hold
// t.mu.
func (t *IPSCTranslator) endReverseDataCallLocked(callControl uint32, now time.Time) {
	t.removeReverseStreamLocked(callControl)
	t.endedData[callControl] = now
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
	defer t.logs.Unlock(&t.mu)
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
func (t *IPSCTranslator) TranslateToIPSC(pkt dmrd.Data) (results [][]byte) {
	t.mu.Lock()
	defer t.logs.Unlock(&t.mu)

	streamID := pkt.Stream

	now := t.now()
	t.pruneLocked(now)

	ss, existed := t.streamLocked(streamID)
	ss.lastSeen = now
	ss.last = pkt
	defer func(id uint32) {
		if !existed && len(results) == 0 {
			t.removeStreamLocked(id)
		}
	}(streamID)

	frameType := pkt.Frame
	dtypeOrVSeq := pkt.Value

	switch frameType {
	case dmrd.FrameDataSync:
		if ss.headerless(enums.DataType(dtypeOrVSeq)) {
			t.logs.Debug("IPSCTranslator: dropping data block outside a data packet",
				"dtype", enums.DataType(dtypeOrVSeq).String())
			return nil
		}
		// Voice LC Header, Terminator, or Data
		switch enums.DataType(dtypeOrVSeq) {
		case enums.DataTypeVoiceLCHeader:
			lc := fullLCFromBurst(&t.logs, pkt, enums.DataTypeVoiceLCHeader)
			copy(ss.lc[:], lc[:9])
			ss.hasLC = true
			ss.privacy = ss.privacy || lcPrivacy(ss.lc)
			// Send voice header (IPSC sends 3 copies)
			for i := 0; i < 3; i++ {
				results = appendPacket(results, t.buildVoiceHeader(pkt, ss, i == 0 && ss.firstPacket, lc))
			}
			ss.headersSent = 3
			ss.firstPacket = false
		case enums.DataTypeTerminatorWithLC:
			results = appendPacket(results, t.buildVoiceTerminator(pkt, ss, fullLCFromBurst(&t.logs, pkt, enums.DataTypeTerminatorWithLC)))
			t.removeStreamLocked(streamID)
		case enums.DataTypeRate34:
			last := ss.lastDataBlock(pkt.Burst)
			// SetRawBurst would send a block that does not decode as its
			// 196 info bits (0x1F), so such a block is dropped here first.
			if _, res := pkt.Burst.Rate34Payload(); res.Uncorrectable {
				t.logs.Debug("IPSCTranslator: dropping rate 3/4 burst with uncorrectable trellis errors")
				if last {
					results = t.endDataCall(pkt, ss, streamID)
				}
				break
			}
			results = appendPacket(results, t.buildIPSCRate34Packet(pkt, ss, last))
			ss.firstPacket = false
			if last {
				t.removeStreamLocked(streamID)
			}
		case enums.DataTypeCSBK, enums.DataTypePIHeader,
			enums.DataTypeDataHeader, enums.DataTypeRate12,
			enums.DataTypeRate1,
			enums.DataTypeMBCHeader, enums.DataTypeMBCContinuation:
			last := ss.lastDataBlock(pkt.Burst)
			burstType, ok := ipscDataBurstType(enums.DataType(dtypeOrVSeq))
			if ok && burstType == ipscBurstPIHeader {
				ss.privacy = true
			}
			var payload [12]byte
			if ok {
				payload, ok = decodeBPTCPayload(pkt.Burst, enums.DataType(dtypeOrVSeq))
			}
			if !ok {
				t.logs.Debug("IPSCTranslator: dropping data burst with no IPSC encoding or uncorrectable BPTC errors",
					"dtype", enums.DataType(dtypeOrVSeq).String())
				if last {
					results = t.endDataCall(pkt, ss, streamID)
				}
				break
			}
			results = appendPacket(results, t.buildIPSCDataPacket(pkt, ss, burstType, payload, last))
			ss.firstPacket = false
			if last {
				t.removeStreamLocked(streamID)
			}
		case enums.DataTypeIdle, enums.DataTypeUnifiedSingleBlock, enums.DataTypeReserved:
			return nil
		default:
			t.logs.Debug("IPSCTranslator: unhandled data sync dtype", "dtype", dtypeOrVSeq)
			return nil
		}

	case dmrd.FrameVoice, dmrd.FrameVoiceSync:
		// Voice burst — decode DMR data and extract AMBE
		data := t.buildVoiceBurst(pkt, ss)
		if data != nil {
			results = append(results, data)
		}

	case dmrd.FrameReserved:
		t.logs.Debug("IPSCTranslator: unknown frame type", "frameType", frameType)
		return nil
	}

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("mmdvm_to_ipsc").Add(float64(len(results)))
	}

	return results
}

// appendPacket appends p to results unless it is nil, as for a packet that
// did not encode.
func appendPacket(results [][]byte, p []byte) [][]byte {
	if p == nil {
		return results
	}
	return append(results, p)
}

// streamLocked returns the MMDVM-to-IPSC state of stream streamID, and
// whether it existed. A new stream is a new call, with the next call
// control and call sequence number. The caller must hold t.mu.
func (t *IPSCTranslator) streamLocked(streamID uint32) (*streamState, bool) {
	if ss, ok := t.streams[streamID]; ok {
		return ss, true
	}
	t.nextCallControl++
	if t.nextCallControl == 0 {
		t.nextCallControl = 1
	}
	ss := &streamState{
		callControl: t.nextCallControl,
		ipscSeq:     t.nextCallSeq,
		firstPacket: true,
	}
	t.nextCallSeq++
	t.streams[streamID] = ss
	if t.metrics != nil {
		t.metrics.TranslatorActiveStreams.WithLabelValues("mmdvm_to_ipsc").Inc()
	}
	return ss, false
}

// EndDataCall ends the data call of the MMDVM stream pkt.Stream when no
// burst of it carries the end flag, such as a call of preamble CSBKs that
// no data follows, and returns the packet that ends it on IPSC, as an XPR
// 8400 ends a call when the signal ends. pkt gives the call fields. It
// returns nothing when no packet of the call was sent.
func (t *IPSCTranslator) EndDataCall(pkt dmrd.Data) [][]byte {
	t.mu.Lock()
	defer t.logs.Unlock(&t.mu)
	ss, ok := t.streams[pkt.Stream]
	if !ok {
		return nil
	}
	results := t.endDataCall(pkt, ss, pkt.Stream)
	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("mmdvm_to_ipsc").Add(float64(len(results)))
	}
	return results
}

// EndStreams ends every MMDVM-to-IPSC call that is still open, as on
// shutdown, and returns the IPSC packets that end them: the voice terminator
// of a voice call, with the LC of the call, and the end packet of a data
// call (see endDataCall). An XPR 8400 that gets no voice terminator goes on
// to send Idle bursts with the carrier up for about 7 s.
func (t *IPSCTranslator) EndStreams() [][]byte {
	t.mu.Lock()
	defer t.logs.Unlock(&t.mu)
	var results [][]byte
	for _, id := range slices.Sorted(maps.Keys(t.streams)) {
		ss := t.streams[id]
		if !isVoiceCallPacket(ss.last) {
			results = append(results, t.endDataCall(ss.last, ss, id)...)
			continue
		}
		lc := pdu.LCBytes(ss.lc)
		if !ss.hasLC {
			lc = constructLC(ss.last.Source, ss.last.Destination, !ss.last.Private)
		}
		term := ss.last
		term.Frame, term.Value = dmrd.FrameDataSync, uint8(enums.DataTypeTerminatorWithLC)
		dmrData, err := layer2.BuildDataBurst(fullLCCodeword(lc, enums.DataTypeTerminatorWithLC),
			enums.DataTypeTerminatorWithLC, t.colorCode, layer2.SyncSourceBS)
		if err != nil {
			t.logs.Debug("IPSCTranslator: cannot build terminator", "error", err)
		}
		term.Burst = dmrData
		results = appendPacket(results, t.buildVoiceTerminator(term, ss, fullLCFromBurst(&t.logs, term, enums.DataTypeTerminatorWithLC)))
		t.removeStreamLocked(id)
	}
	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("mmdvm_to_ipsc").Add(float64(len(results)))
	}
	return results
}

// isVoiceCallPacket reports whether pkt is a packet of a voice call: a
// voice burst, or a voice LC header or PI header.
func isVoiceCallPacket(pkt dmrd.Data) bool {
	switch pkt.Frame {
	case dmrd.FrameVoice, dmrd.FrameVoiceSync:
		return true
	case dmrd.FrameDataSync:
		return pkt.Value == uint8(enums.DataTypeVoiceLCHeader) || pkt.Value == uint8(enums.DataTypePIHeader)
	case dmrd.FrameReserved:
	}
	return false
}

// CleanupStream removes state for an MMDVM stream ID in both directions
// (e.g. on timeout): the MMDVM-to-IPSC stream with that ID and any
// IPSC-to-MMDVM stream that was assigned that ID.
func (t *IPSCTranslator) CleanupStream(streamID uint32) {
	t.mu.Lock()
	defer t.logs.Unlock(&t.mu)
	t.removeStreamLocked(streamID)
	for callControl, rss := range t.reverseStreams {
		if rss.streamID == streamID {
			t.removeReverseStreamLocked(callControl)
		}
	}
}

// newUser returns the IPSC packet of the call ss for pkt: a data packet
// (0x83 or 0x84) when isData and a voice packet (0x80 or 0x81) otherwise,
// with the end flag when isEnd and the RTP marker and payload type given.
// It takes the next RTP sequence number and timestamp of the call.
func (t *IPSCTranslator) newUser(pkt dmrd.Data, ss *streamState, isEnd, isData, marker bool, payloadType uint8) ipscpkt.User {
	op := ipscpkt.OpGroupVoice
	switch {
	case isData && pkt.Private:
		op = ipscpkt.OpPrivateData
	case isData:
		op = ipscpkt.OpGroupData
	case pkt.Private:
		op = ipscpkt.OpPrivateVoice
	}
	// MOTOTRBO repeaters send call priority 0x02 on group and private voice,
	// and 0x01 on group and private data.
	priority := uint8(0x02)
	if isData {
		priority = 0x01
	}
	var info uint8
	if ss.privacy {
		info |= ipscCallInfoPrivacy
	}
	if pkt.Slot == enums.Slot2 {
		info |= ipscCallInfoSlot2
	}
	if isEnd {
		info |= ipscCallInfoEnd
	}
	u := ipscpkt.User{
		Opcode:      op,
		ID:          t.peerID,
		CallSeq:     ss.ipscSeq,
		Source:      pkt.Source,
		Destination: pkt.Destination,
		Priority:    priority,
		CallControl: ss.callControl,
		Info:        ipscpkt.CallInfo(info),
		RTP: ipscpkt.RTP{
			Flags: 0x80, Marker: marker, PayloadType: payloadType,
			Seq: ss.rtpSeq, Timestamp: ss.rtpTimestamp,
		},
	}
	ss.rtpSeq++
	ss.rtpTimestamp += rtpTimestampIncrement
	return u
}

// encodeUser returns the octets of u, or nil when it does not encode.
func (t *IPSCTranslator) encodeUser(u *ipscpkt.User) []byte {
	b, err := u.AppendBinary(make([]byte, 0, 66))
	if err != nil {
		t.logs.Debug("IPSCTranslator: cannot encode IPSC packet", "error", err)
		return nil
	}
	return b
}

// buildDataBurst sets the burst of u to the data burst raw of type dt and
// returns the octets of u. payload, when not nil, takes the place of the
// BPTC payload of raw, so the packet carries it even when raw does not
// decode. The slot type of the packet gets the color code of raw's slot
// type, or 0 when it does not decode. A voice LC header or terminator
// leaves the RSSI status bit of the status octet clear, as an XPR 8400
// does on the voice headers it sends with no RSSI and as node-dmr-lib's
// IPSCPeer does; other data packets set it with a zero RSSI.
func (t *IPSCTranslator) buildDataBurst(u *ipscpkt.User, raw layer2.RawBurst, dt enums.DataType, payload *[12]byte) []byte {
	cc := slotTypeColorCode(raw)
	if payload != nil {
		var err error
		raw, err = layer2.BuildDataBurst(*payload, dt, cc, layer2.SyncSourceBS)
		if err != nil {
			t.logs.Debug("IPSCTranslator: cannot carry data burst", "dtype", dt.String(), "error", err)
			return nil
		}
	}
	if _, err := u.SetRawBurst(raw, layer2.Hint{Kind: layer2.KindData, DataType: dt, HasDataType: true}); err != nil {
		t.logs.Debug("IPSCTranslator: cannot carry data burst", "dtype", dt.String(), "error", err)
		return nil
	}
	if dt == enums.DataTypeVoiceLCHeader || dt == enums.DataTypeTerminatorWithLC {
		u.Data.Status &^= ipscStatusRSSI
	}
	var st pdu.SlotType
	st.ColorCode, st.DataType = cc, dt
	if info, err := st.Info(); err == nil {
		u.Data.Trailer[1] = byte(info)
	}
	return t.encodeUser(u)
}

// buildVoiceHeader builds the IPSC voice header packet of pkt, which
// carries the full LC lc.
func (t *IPSCTranslator) buildVoiceHeader(pkt dmrd.Data, ss *streamState, isFirst bool, lc [12]byte) []byte {
	u := t.newUser(pkt, ss, false, false, isFirst, 0x5D)
	return t.buildDataBurst(&u, pkt.Burst, enums.DataTypeVoiceLCHeader, &lc)
}

// buildVoiceTerminator builds the IPSC voice terminator packet of pkt,
// which carries the full LC lc.
func (t *IPSCTranslator) buildVoiceTerminator(pkt dmrd.Data, ss *streamState, lc [12]byte) []byte {
	u := t.newUser(pkt, ss, true, false, false, 0x5E)
	return t.buildDataBurst(&u, pkt.Burst, enums.DataTypeTerminatorWithLC, &lc)
}

// headerless reports whether dataType is a rate 1/2, 3/4 or 1 data block
// and the call has no data packet in progress for it: its header failed
// its CRC or never came, or its packet already ended. MMDVMHost discards
// such a block from the network (CDMRSlot::writeNetwork).
func (ss *streamState) headerless(dataType enums.DataType) bool {
	return ss.data.headerless(dataType)
}

// lastDataBlock adds a data burst to the data packet of the call and
// reports whether it completes the packet (see DataBurstEndsCall).
func (ss *streamState) lastDataBlock(data [33]byte) bool {
	return DataBurstEndsCall(&ss.data, data)
}

// DataCall follows the data packet of one call to find its last block.
type DataCall struct {
	asm *packet.Assembler
	// pHead is set after a first header with the proprietary SAP, which a
	// P_HEAD should follow.
	pHead bool
}

// headerless reports whether dataType is a rate 1/2, 3/4 or 1 data block
// and c has no data packet in progress for it.
func (c *DataCall) headerless(dataType enums.DataType) bool {
	switch dataType { //nolint:exhaustive // only data blocks belong to a packet
	case enums.DataTypeRate12, enums.DataTypeRate34, enums.DataTypeRate1:
		return c.asm == nil || !c.asm.InProgress()
	}
	return false
}

// DataBurstEndsCall adds the data burst data, a data header or a rate 1/2,
// 3/4 or 1 block, to c, which holds the data packet of one call so far,
// and reports whether the burst completes the packet: a header with no
// blocks to follow, or the last block. A packet whose CRC fails is complete
// too, and a block that does not decode still counts. Other bursts, such as
// CSBKs, never complete a packet.
//
// The Blocks To Follow of a first header with SAP 9 counts the P_HEAD that
// follows it (ETSI TS 102 361-1 8.2.1 and 8.2.2.1). MMDVMHost does not pass
// the P_HEAD of an RF packet to the network, so when a data block follows
// such a header directly, the P_HEAD is counted as if it had arrived.
func DataBurstEndsCall(c *DataCall, data [33]byte) bool {
	b, err := layer2.NewBurstFromBytes(data)
	if err != nil {
		return false
	}
	if c.asm == nil {
		c.asm = packet.NewAssembler(127)
	}
	pHead := c.pHead
	c.pHead = false
	if b.Kind == layer2.KindData {
		switch d := b.Data.(type) {
		case *pdu.DataHeader:
			c.pHead = announcesPHead(d)
		case *pdu.Rate12Data, *pdu.Rate34Data, *pdu.Rate1Data:
			if pHead {
				var h pdu.DataHeader
				if h.SetProprietary(pdu.ProprietaryHeader{SAP: enums.SAPProprietaryPacketData}) == nil {
					_, _, _ = c.asm.Add(&h)
				}
			}
		}
	}
	_, done, err := c.asm.AddBurst(b)
	return done || errors.Is(err, packet.ErrBlocksMissing)
}

// announcesPHead reports whether h is the first header of a proprietary
// packet: an unconfirmed or confirmed data header with SAP 9.
func announcesPHead(h *pdu.DataHeader) bool {
	if u, ok := h.AsUnconfirmed(); ok {
		return u.SAP == enums.SAPProprietaryPacketData
	}
	if c, ok := h.AsConfirmed(); ok {
		return c.SAP == enums.SAPProprietaryPacketData
	}
	return false
}

// dataPayloadType returns the RTP payload type of a data packet: 0x5E for
// the last packet of a data call, as for a voice terminator, and 0x5D
// otherwise, as an XPR 8400 sends them.
func dataPayloadType(last bool) byte {
	if last {
		return 0x5E
	}
	return 0x5D
}

// buildIPSCDataPacket builds the IPSC packet for a burst with a
// BPTC(196,96) payload, such as a CSBK or a data header. Data bursts go in
// data packets (0x83/0x84); a PI header belongs to a voice call, and an XPR
// 8400 sends it in a voice packet (0x80/0x81).
func (t *IPSCTranslator) buildIPSCDataPacket(pkt dmrd.Data, ss *streamState, burstType byte, payload [12]byte, last bool) []byte {
	u := t.newUser(pkt, ss, last, burstType != ipscBurstPIHeader, ss.firstPacket, dataPayloadType(last))
	return t.buildDataBurst(&u, pkt.Burst, enums.DataType(burstType), &payload)
}

// endDataCall ends the data call of stream streamID when no packet of it
// carries the end flag, such as when its last block was dropped, and
// returns the end packet that tells the IPSC peers, or nothing when no
// packet of the call was sent.
func (t *IPSCTranslator) endDataCall(pkt dmrd.Data, ss *streamState, streamID uint32) [][]byte {
	t.removeStreamLocked(streamID)
	if ss.firstPacket {
		return nil
	}
	return appendPacket(nil, t.buildIPSCDataEndPacket(pkt, ss))
}

// buildIPSCDataEndPacket builds the 34-byte packet that an XPR 8400 sends
// to end a data call when it loses the signal: a "sync not detected"
// placeholder (burst type 0x13) with the end flag and RTP payload type
// 0x5E, and the timeslot in bit 7 of byte 31.
func (t *IPSCTranslator) buildIPSCDataEndPacket(pkt dmrd.Data, ss *streamState) []byte {
	u := t.newUser(pkt, ss, true, true, false, 0x5E)
	u.Burst = ipscpkt.BurstSyncLost
	if pkt.Slot == enums.Slot2 {
		u.Data.Status = 0x80
	}
	return t.encodeUser(&u)
}

// buildIPSCRate34Packet builds the IPSC rate 3/4 data packet of pkt. An
// XPR 8400 carries the 18 octets of a rate 3/4 block (ETSI TS 102 361-1
// 9.2.10) rotated left by two, which SetRawBurst does.
func (t *IPSCTranslator) buildIPSCRate34Packet(pkt dmrd.Data, ss *streamState, last bool) []byte {
	u := t.newUser(pkt, ss, last, true, ss.firstPacket, dataPayloadType(last))
	return t.buildDataBurst(&u, pkt.Burst, enums.DataTypeRate34, nil)
}

// slotTypeColorCode returns the color code from the slot type of a DMR data
// burst, or 0 when the slot type cannot be decoded.
func slotTypeColorCode(data layer2.RawBurst) enums.ColorCode {
	st := data.SlotType()
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
func mmdvmVoiceBurstPosition(pkt dmrd.Data) (int, bool) {
	if pkt.Frame == dmrd.FrameVoiceSync {
		return burstA, true
	}
	if pkt.Value < burstB || pkt.Value > burstF {
		return 0, false
	}
	return int(pkt.Value), true
}

// voiceBurst returns the voice burst type of superframe position pos (0-5
// for A-F).
func voiceBurst(pos int) enums.VoiceBurstType {
	return enums.VoiceBurstA + enums.VoiceBurstType(pos) //nolint:gosec // pos is 0-5
}

// embByte encodes the IPSC EMB byte (color code, PI, and LCSS, FEC removed)
// that follows the embedded data in bursts B-F. It uses the color code and
// LCSS of the burst's decoded EMB when available, otherwise the LCSS for
// the burst position. PI, the pre-emption and power control indicator, is
// always clear, as in ipsc2hbpc.
func embByte(burst *layer2.Burst, pos int) byte {
	lcss, _ := voiceBurst(pos).LCSS()
	emb := pdu.EmbeddedSignalling{LCSS: lcss}
	if burst.HasEmbeddedSignalling() && !burst.EmbeddedSignalling.FEC.Uncorrectable {
		emb = burst.EmbeddedSignalling
	}
	info, _ := emb.Info()
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

// buildVoiceBurst builds the IPSC voice burst packet of pkt: the AMBE
// frames, and for bursts B to F the embedded signalling and the EMB of
// embByte, with the full LC of the call in burst E.
func (t *IPSCTranslator) buildVoiceBurst(pkt dmrd.Data, ss *streamState) []byte {
	// Decode the DMR burst to read its EMB. The DMRD frame type says the
	// burst is voice, so its SYNC field does not classify it.
	_ = t.burst.DecodeFromBytesWith(pkt.Burst, mmdvmVoiceDecodeOptions(pkt))

	burstIdx, ok := mmdvmVoiceBurstPosition(pkt)
	if !ok {
		t.logs.Debug("IPSCTranslator: invalid voice sequence", "frameType", pkt.Frame, "vseq", pkt.Value)
		return nil
	}
	emb := embByte(&t.burst, burstIdx)
	call, learned := t.lateEntryLCLocked(ss, pkt)

	u := t.newUser(pkt, ss, false, false, false, 0x5D)
	if burstIdx == burstE {
		// The call's full LC with the destination and source of the
		// packet. A call that started without a voice header uses the LC
		// of its embedded signalling.
		if !ss.hasLC && learned && call.Group == !pkt.Private {
			ss.lc, ss.hasLC = call.LC, true
			ss.privacy = ss.privacy || lcPrivacy(call.LC)
		}
		lc := pdu.LCBytes(ss.lc)
		if !ss.hasLC {
			lc = constructLC(pkt.Source, pkt.Destination, !pkt.Private)
		}
		_ = lc.SetAddresses(lcAddress(pkt.Destination), lcAddress(pkt.Source)) // lcAddress keeps 24 bits
		u.Voice.LC = lc
	}
	if _, err := u.SetRawBurst(pkt.Burst, layer2.Hint{Kind: layer2.KindVoice, VoiceBurst: voiceBurst(burstIdx)}); err != nil {
		t.logs.Debug("IPSCTranslator: cannot carry voice burst", "error", err)
		return nil
	}
	if burstIdx != burstA {
		u.Voice.EMB = pdu.EMBInfo(emb)
		if !t.burst.HasEmbeddedSignalling() {
			u.Voice.Embedded = [4]byte{}
		}
	}
	return t.encodeUser(&u)
}

// lateEntryLCLocked runs the MMDVM voice burst pkt through the voice
// receiver of the call ss, and returns the call when the receiver learned
// its LC from the embedded LC of bursts B to E of one superframe, a late
// entry. An XPR 8400 builds the voice header of such a call from the LC
// that burst E carries in bytes 56-64. The caller must hold t.mu.
func (t *IPSCTranslator) lateEntryLCLocked(ss *streamState, pkt dmrd.Data) (voice.Call, bool) {
	if ss.voice == nil {
		var err error
		ss.voice, err = voice.NewReceiver(&voice.ReceiverConfig{Lost: -1, Hang: -1, Scratch: t.scratch})
		if err != nil {
			t.logs.Debug("IPSCTranslator: cannot make voice receiver", "error", err)
			return voice.Call{}, false
		}
	}
	now := time.Duration(t.now().UnixNano())
	t.events = ss.voice.Burst(now, &pkt.Burst, pkt.Hint(now), t.events[:0])
	for _, e := range t.events {
		if e.Call.Origin == voice.OriginEmbedded {
			return e.Call, true
		}
	}
	return voice.Call{}, false
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
func constructFullLC(src, dst uint32, groupCall bool, dataType enums.DataType) [12]byte {
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
func constructLC(src, dst uint32, groupCall bool) pdu.LCBytes {
	var lc pdu.LCBytes
	_ = lc.SetGroup(groupCall)
	lc.SetFID(enums.FIDStandardized)
	_ = lc.SetServiceOptions(layer3.MotorolaServiceOptions())
	_ = lc.SetAddresses(lcAddress(dst), lcAddress(src)) // lcAddress keeps 24 bits
	return lc
}

// lcPrivacy reports whether lc is a group or unit-to-unit voice LC with the
// privacy service option.
func lcPrivacy(lc [9]byte) bool {
	flc := pdu.DecodeFullLinkControlBytes(lc)
	if g, ok := flc.AsGroupVoice(); ok {
		return g.ServiceOptions.IsPrivacy
	}
	if u, ok := flc.AsUnitToUnit(); ok {
		return u.ServiceOptions.IsPrivacy
	}
	return false
}

// lcAddress returns the low 24 bits of id, the part of it that the DMRD and
// IPSC headers carry.
func lcAddress(id uint32) uint32 {
	return id & 0xFFFFFF
}

// mmdvmVoiceDecodeOptions returns the decode options for the burst of an
// MMDVM voice packet: a voice burst at the superframe position that
// mmdvmVoiceBurstPosition gives.
func mmdvmVoiceDecodeOptions(pkt dmrd.Data) *layer2.DecodeOptions {
	opts := &layer2.DecodeOptions{Kind: layer2.KindVoice}
	if pos, ok := mmdvmVoiceBurstPosition(pkt); ok {
		opts.VoiceBurst = voiceBurst(pos)
	}
	return opts
}

// fullLCFromBurst returns the full LC for an MMDVM voice LC header or
// terminator burst. The feature set ID and service options come from the LC
// in the burst when it is valid; the call type, destination, and source come
// from the packet fields, which a rewrite rule may have changed.
func fullLCFromBurst(logs *logging.Queue, pkt dmrd.Data, dataType enums.DataType) [12]byte {
	return fullLCCodeword(rewrittenLC(logs, &pkt, dataType), dataType)
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
	src, dst  uint32
	groupCall bool
	slot      enums.Slot
	headerLC  [12]byte
	hasLC     bool

	// data is the data packet of the call so far, sent to MMDVM.
	data DataCall
}

// rtpMaxForward is the largest forward RTP timestamp step (8 kHz units,
// 1 second) that still continues a call, as in ipsc2hbpc's
// OUT_RTP_MAX_FWD. A larger step is a new call that reuses the call
// control. Unlike ipsc2hbpc, a backward step this small is a reordered
// packet, not a new call.
const rtpMaxForward = 8000

// TranslateToMMDVM converts raw IPSC user packet data into MMDVM DMRD Packets.
// Returns nil if the packet cannot be translated.
func (t *IPSCTranslator) TranslateToMMDVM(packetType byte, data []byte) (results []dmrd.Data) {
	t.mu.Lock()
	defer t.logs.Unlock(&t.mu)

	now := t.now()
	t.pruneLocked(now)

	u, err := ipscpkt.DecodeUser(data)
	if err != nil {
		t.logs.Debug("IPSCTranslator: ignoring IPSC packet", "type", packetType, "length", len(data), "error", err)
		return nil
	}
	src, dst := u.Source, u.Destination
	groupCall := u.Opcode == ipscpkt.OpGroupVoice || u.Opcode == ipscpkt.OpGroupData
	slot := u.Info.Slot()
	isEnd := u.Info.End()

	t.logs.Debug("IPSCTranslator: TranslateToMMDVM",
		"packetType", fmt.Sprintf("0x%02X", packetType),
		"src", src, "dst", dst, "groupCall", groupCall,
		"slot", slot, "isEnd", isEnd)

	burstType := byte(u.Burst)

	if burstType&0x7F == ipscBurstSyncUndetect {
		t.syncLostLocked(&u, now)
		return nil
	}
	if !u.IsVoice() && u.Data.Bits < payloadBits(burstType) {
		t.logs.Debug("IPSCTranslator: IPSC burst too short",
			"burstType", fmt.Sprintf("0x%02X", burstType), "bits", u.Data.Bits)
		return nil
	}

	// The call control identifies the stream.
	callControl := u.CallControl

	rss, existed := t.reverseStreams[callControl]
	if !existed {
		if rss = t.newReverseStreamLocked(callControl, burstType); rss == nil {
			return nil
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
		results = append(results, t.checkRTPContinuity(rss, u.RTP.Timestamp)...)
	}
	sent := len(results)

	switch burstType {
	case ipscBurstVoiceHead:
		// Voice follows the header with burst A, so the header holds the
		// place of a burst F.
		rss.anchorPos, rss.anchorRTPSeq, rss.hasAnchor = burstF, u.RTP.Seq, true
		rss.hasLast = false
		// Voice LC Header — only process the first one (IPSC sends 3)
		if !rss.started {
			pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
				enums.DataTypeVoiceLCHeader, [12]byte(u.Data.Octets[:12]))
			results = append(results, pkt)
			rss.started = true
		}
		// Skip duplicate headers

	case ipscBurstVoiceTerm:
		// Voice Terminator
		pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
			enums.DataTypeTerminatorWithLC, [12]byte(u.Data.Octets[:12]))
		results = append(results, pkt)
		t.removeReverseStreamLocked(callControl)

	case ipscBurstSlot1, ipscBurstSlot2:
		// Voice burst — extract AMBE, FEC-encode, build DMR burst
		pkts := t.buildMMDVMVoiceBurst(src, dst, groupCall, slot, rss, &u)
		results = append(results, pkts...)

	case ipscBurstPIHeader, ipscBurstCSBK, ipscBurstMBCHeader,
		ipscBurstMBCContinuation, ipscBurstDataHeader, ipscBurstRate12, ipscBurstRate34,
		ipscBurstIdle, ipscBurstUSBD, ipscBurstRaw:
		if pkt, ok := t.reverseDataBurstLocked(src, dst, groupCall, slot, rss, &u); ok {
			results = append(results, pkt)
		}

	default:
		t.logs.Debug("IPSCTranslator: unsupported IPSC burst type", "burstType", fmt.Sprintf("0x%02X", burstType))
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
		t.endReverseCallLocked(u.Opcode, callControl, now)
	}

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("ipsc_to_mmdvm").Add(float64(len(results)))
	}

	return results
}

// payloadBits returns the payload size in bits that a data packet of
// burstType needs: 144 for a rate 3/4 block, 196 for a raw block and 96 for
// the others.
func payloadBits(burstType byte) uint16 {
	switch burstType {
	case ipscBurstRate34:
		return 144
	case ipscBurstRaw:
		return 196
	}
	return 96
}

// syncLostLocked handles u, a "sync not detected" packet, at now: the repeater lost a burst of
// the call, and the next one starts a new superframe at burst A. The end
// flag ends a data call. A MOTOTRBO repeater also sets it on this packet in
// a voice call when it loses sync in the middle of the call, so a voice
// call goes on. The caller must hold t.mu.
func (t *IPSCTranslator) syncLostLocked(u *ipscpkt.User, now time.Time) {
	rss, ok := t.reverseStreams[u.CallControl]
	if !ok {
		return
	}
	rss.lastSeen = now
	superframeRestartsAfter(rss, u.RTP.Seq)
	if u.Info.End() && isDataPacket(u.Opcode) {
		t.endReverseDataCallLocked(u.CallControl, now)
	}
}

// reverseDataBurstLocked builds the MMDVM packet for the IPSC data burst
// data of burstType in the call of rss, and adds it to the data packet of
// the call. It drops a rate 1/2 or 3/4 block that is not part of a data
// packet in progress. An XPR 8400 sends a copy of the last preamble CSBK in
// place of a Defined Short Data header, so the blocks of such a message
// arrive with no header. MMDVMHost discards them. The caller must hold
// t.mu.
func (t *IPSCTranslator) reverseDataBurstLocked(src, dst uint32, groupCall bool, slot enums.Slot, rss *reverseStreamState, u *ipscpkt.User) (dmrd.Data, bool) {
	burstType := byte(u.Burst)
	if rss.data.headerless(enums.DataType(burstType)) {
		t.logs.Debug("IPSCTranslator: dropping data block outside a data packet",
			"burstType", fmt.Sprintf("0x%02X", burstType))
		return dmrd.Data{}, false
	}
	var pkt dmrd.Data
	switch burstType {
	case ipscBurstRate34:
		pkt = t.buildMMDVMRate34Packet(src, dst, groupCall, slot, rss, u)
	case ipscBurstRaw:
		var ok bool
		if pkt, ok = t.buildMMDVMRawPacket(src, dst, groupCall, slot, rss, u); !ok {
			return dmrd.Data{}, false
		}
	default:
		payload := [12]byte(u.Data.Octets[:12])
		if !layer2.MMDVMHostAccepts(enums.DataType(burstType), payload) {
			t.logs.Debug("IPSCTranslator: passing on a data burst that fails its CRC",
				"burstType", fmt.Sprintf("0x%02X", burstType))
		}
		pkt = t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss, enums.DataType(burstType), payload)
	}
	DataBurstEndsCall(&rss.data, pkt.Burst)
	return pkt, true
}

// isDataPacket reports whether op is a group or private data packet.
func isDataPacket(op ipscpkt.Opcode) bool {
	return op == ipscpkt.OpGroupData || op == ipscpkt.OpPrivateData
}

// newReverseStreamLocked starts the IPSC-to-MMDVM stream of the call
// callControl, whose first packet has burstType, and returns it. It returns
// nil for a data block of a data call that ended with the end flag: it has
// no header to start a call with, and MMDVMHost discards such a block. The
// caller must hold t.mu.
func (t *IPSCTranslator) newReverseStreamLocked(callControl uint32, burstType byte) *reverseStreamState {
	if _, ended := t.endedData[callControl]; ended && isDataBlock(burstType) {
		t.logs.Debug("IPSCTranslator: dropping data block after the end of its call",
			"burstType", fmt.Sprintf("0x%02X", burstType), "callControl", callControl)
		return nil
	}
	rss := &reverseStreamState{streamID: t.newStreamIDLocked()}
	t.reverseStreams[callControl] = rss
	if t.metrics != nil {
		t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Inc()
	}
	return rss
}

// endReverseCallLocked ends the IPSC-to-MMDVM stream of the call
// callControl, whose packet with opcode op carried the end flag at now. The
// caller must hold t.mu.
func (t *IPSCTranslator) endReverseCallLocked(op ipscpkt.Opcode, callControl uint32, now time.Time) {
	if isDataPacket(op) {
		t.endReverseDataCallLocked(callControl, now)
		return
	}
	t.removeReverseStreamLocked(callControl)
}

// isDataBlock reports whether burstType is a data block that continues a
// packet or MBC and so cannot start a call: a rate 1/2 or 3/4 block or an
// MBC continuation.
func isDataBlock(burstType byte) bool {
	return burstType == ipscBurstRate12 || burstType == ipscBurstRate34 || burstType == ipscBurstMBCContinuation
}

// ReverseStreamID returns the MMDVM stream ID of the IPSC call that data,
// an IPSC user packet, belongs to, if the call has one.
func (t *IPSCTranslator) ReverseStreamID(data []byte) (uint32, bool) {
	u, err := ipscpkt.DecodeUser(data)
	if err != nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.logs.Unlock(&t.mu)
	rss, ok := t.reverseStreams[u.CallControl]
	if !ok {
		return 0, false
	}
	return rss.streamID, true
}

// EndsDataCall reports whether data, an IPSC user packet of packetType, is
// the last packet of a data call: a group or private data packet with the
// end flag. An XPR 8400 sets it on the last block of a data packet, and on
// a header with no blocks to follow such as an ACK.
func EndsDataCall(packetType byte, data []byte) bool {
	u, err := ipscpkt.DecodeUser(data)
	return err == nil && isDataPacket(ipscpkt.Opcode(packetType)) && u.Info.End()
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
func (t *IPSCTranslator) checkRTPContinuity(rss *reverseStreamState, ts uint32) []dmrd.Data {
	step := ts - rss.rtpTimestamp
	switch {
	case !rss.hasTimestamp || step <= rtpMaxForward:
		rss.rtpTimestamp, rss.hasTimestamp = ts, true
		return nil
	case -step <= rtpMaxForward:
		return nil
	default:
		t.logs.Debug("IPSCTranslator: RTP discontinuity, starting a new stream", "streamID", rss.streamID)
		out := t.restartReverseStream(rss)
		rss.rtpTimestamp, rss.hasTimestamp = ts, true
		return out
	}
}

// restartReverseStream ends the MMDVM stream of rss with a terminator, if
// it sent any voice, and resets rss to a new stream. MMDVMHost ignores a
// voice header while a network call is active, so without the terminator
// the new call would not start.
func (t *IPSCTranslator) restartReverseStream(rss *reverseStreamState) []dmrd.Data {
	var out []dmrd.Data
	if rss.voiceSent {
		lc := constructFullLC(rss.src, rss.dst, rss.groupCall, enums.DataTypeTerminatorWithLC)
		if rss.hasLC {
			lc = rss.headerLC
			_ = layer2.MMDVMHostSeal(enums.DataTypeTerminatorWithLC, &lc) // never fails for a terminator
		}
		dmrData, err := layer2.BuildDataBurst(lc, enums.DataTypeTerminatorWithLC, t.colorCode, layer2.SyncSourceBS)
		if err != nil {
			t.logs.Debug("IPSCTranslator: cannot build terminator", "error", err)
		}
		out = append(out, dmrd.Data{
			Seq:         rss.seq,
			Source:      rss.src,
			Destination: rss.dst,
			Repeater:    t.repeaterID,
			Slot:        rss.slot,
			Private:     !rss.groupCall,
			Frame:       dmrd.FrameDataSync,
			Value:       uint8(enums.DataTypeTerminatorWithLC),
			Stream:      rss.streamID,
			Burst:       dmrData,
		})
	}
	*rss = reverseStreamState{streamID: t.newStreamIDLocked(), lastSeen: rss.lastSeen}
	return out
}

// buildMMDVMDataPacket builds an MMDVM DMRD packet for a voice LC header, terminator,
// or data burst (CSBK, Data Header, etc.).
// It constructs the 33-byte DMR burst from the IPSC payload data using BPTC encoding.
func (t *IPSCTranslator) buildMMDVMDataPacket(
	src, dst uint32, groupCall bool, slot enums.Slot,
	rss *reverseStreamState,
	dataType enums.DataType,
	lcBytes [12]byte,
) dmrd.Data {
	pkt := dmrd.Data{
		Seq:         rss.seq,
		Source:      src,
		Destination: dst,
		Repeater:    t.repeaterID,
		Slot:        slot,
		Private:     !groupCall,
		Frame:       dmrd.FrameDataSync,
		Value:       uint8(dataType),
		Stream:      rss.streamID,
	}
	rss.seq++

	// Voice LC headers and terminators pass through when their parity is
	// valid, so flags such as emergency, privacy, and the protect flag survive.
	if (dataType == enums.DataTypeVoiceLCHeader || dataType == enums.DataTypeTerminatorWithLC) &&
		!layer2.MMDVMHostAccepts(dataType, lcBytes) {
		if lc, ok := lateEntryLC(lcBytes, src, dst, groupCall); ok {
			lcBytes = fullLCCodeword(lc, dataType)
		} else {
			t.logs.Debug("IPSCTranslator: invalid full LC from IPSC, constructing from packet fields",
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
		t.logs.Debug("IPSCTranslator: cannot build data burst", "dtype", dataType.String(), "error", err)
	}
	pkt.Burst = dmrData

	return pkt
}

// lateEntryLC returns the LC of a voice header that a MOTOTRBO repeater
// built from the embedded LC of a call it joined late. Such a header carries
// the 9 LC octets with zero RS(12,9) parity (and 0x10 0x00 in bytes 34-35
// instead of 0x80 0x0A). The LC is used when its call type and addresses
// match the IPSC header, so its feature set ID and service options survive.
func lateEntryLC(payload [12]byte, src, dst uint32, groupCall bool) (pdu.LCBytes, bool) {
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

// buildMMDVMRate34Packet builds an MMDVM DMRD packet for the IPSC rate 3/4
// data packet u, whose 18-octet block is rotated left by two (see
// buildIPSCRate34Packet). The burst gets the color code of the bridge, not
// the one in the slot type of the packet.
func (t *IPSCTranslator) buildMMDVMRate34Packet(src, dst uint32, groupCall bool, slot enums.Slot, rss *reverseStreamState, u *ipscpkt.User) dmrd.Data {
	v := *u
	v.Data.Flags[1] &^= ipscFlagSlotType
	dmrData, err := v.RawBurst(t.colorCode, layer2.SyncSourceBS)
	if err != nil {
		t.logs.Debug("IPSCTranslator: cannot build rate 3/4 burst", "error", err)
	}
	pkt := dmrd.Data{
		Seq:         rss.seq,
		Source:      src,
		Destination: dst,
		Repeater:    t.repeaterID,
		Slot:        slot,
		Private:     !groupCall,
		Frame:       dmrd.FrameDataSync,
		Value:       uint8(enums.DataTypeRate34),
		Stream:      rss.streamID,
		Burst:       dmrData,
	}
	rss.seq++
	return pkt
}

func (t *IPSCTranslator) buildMMDVMRawPacket(src, dst uint32, groupCall bool, slot enums.Slot, rss *reverseStreamState, u *ipscpkt.User) (dmrd.Data, bool) {
	st, ok := u.Data.SlotType()
	if !ok {
		t.logs.Debug("IPSCTranslator: dropping raw block with no slot type")
		return dmrd.Data{}, false
	}
	dt := st.DataType()
	raw, err := u.RawBurst(t.colorCode, layer2.SyncSourceBS)
	if err == nil {
		err = raw.SetSlotType(pdu.SlotType{ColorCode: t.colorCode, DataType: dt})
	}
	if err != nil {
		t.logs.Debug("IPSCTranslator: cannot build raw block", "error", err)
		return dmrd.Data{}, false
	}
	pkt := dmrd.Data{
		Seq:         rss.seq,
		Source:      src,
		Destination: dst,
		Repeater:    t.repeaterID,
		Slot:        slot,
		Private:     !groupCall,
		Frame:       dmrd.FrameDataSync,
		Value:       uint8(dt),
		Stream:      rss.streamID,
		Burst:       raw,
	}
	rss.seq++
	return pkt, true
}

// buildMMDVMVoiceBurst builds the MMDVM DMRD packet of the IPSC voice burst
// u: its AMBE frames with a flagged frame as silence, the BS-sourced voice
// SYNC at burst A, and at bursts B to F the embedded signalling of the
// packet with the EMB of populateEmbeddedSignalling.
func (t *IPSCTranslator) buildMMDVMVoiceBurst(
	src, dst uint32, groupCall bool, slot enums.Slot,
	rss *reverseStreamState,
	u *ipscpkt.User,
) []dmrd.Data {
	frames, bad := vocoder.UnpackAMBEVoiceFlags(u.Voice.AMBE)
	bad[0] = u.Voice.Control&ipscVoiceCtrlBadFrame1 != 0
	for i := range frames {
		if bad[i] {
			frames[i] = vocoder.AMBESilence()
		}
	}

	burstIdx, ok := ipscVoiceBurstPosition(rss, u)
	if !ok {
		t.logs.Debug("IPSCTranslator: cannot place voice burst in superframe", "layout", u.Voice.Layout)
		return nil
	}

	v := *u
	var err error
	if v.Voice.AMBE, err = vocoder.PackAMBEVoice(frames); err != nil {
		t.logs.Debug("IPSCTranslator: cannot pack voice burst", "error", err)
		return nil
	}
	// Burst A carries the BS-sourced voice SYNC, as the data bursts carry
	// BS-sourced data SYNC: the repeater is a BS, and a duplex MMDVMHost
	// sends BS-sourced SYNC to the network too.
	if burstIdx == burstA {
		v.Voice.Layout = ipscpkt.VoiceLayoutA
	} else if v.Voice.EMB, err = t.populateEmbeddedSignalling(burstIdx); err != nil {
		t.logs.Debug("IPSCTranslator: cannot build EMB", "error", err)
		return nil
	}
	dmrData, err := v.RawBurst(t.colorCode, layer2.SyncSourceBS)
	if err != nil {
		t.logs.Debug("IPSCTranslator: cannot encode voice burst", "error", err)
		return nil
	}

	frameType := dmrd.FrameVoice
	if burstIdx == burstA {
		frameType = dmrd.FrameVoiceSync
	}

	pkt := dmrd.Data{
		Seq:         rss.seq,
		Source:      src,
		Destination: dst,
		Repeater:    t.repeaterID,
		Slot:        slot,
		Private:     !groupCall,
		Frame:       frameType,
		Value:       uint8(burstIdx), //nolint:gosec // 0 to 5
		Stream:      rss.streamID,
		Burst:       dmrData,
	}
	rss.seq++

	return []dmrd.Data{pkt}
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
func ipscVoiceBurstPosition(rss *reverseStreamState, u *ipscpkt.User) (int, bool) {
	seq := u.RTP.Seq
	if rss.hasLast && seq == rss.lastRTPSeq {
		return 0, false
	}
	rss.lastRTPSeq, rss.hasLast = seq, true
	anchor := -1
	switch {
	case u.Voice.Layout == ipscpkt.VoiceLayoutA || u.Voice.Control&ipscVoiceCtrlSync != 0:
		anchor = burstA
	case u.Voice.Layout == ipscpkt.VoiceLayoutE:
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
	if u.Info.End() {
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

// populateEmbeddedSignalling returns the EMB of voice burst burstIdx (B-F)
// for an IPSC voice burst. MOTOTRBO repeaters put the 32-bit embedded
// fragment and the EMB in the packet, but on a weak signal the EMB is
// wrong: the LCSS follows the burst position, as every voice superframe
// uses the same LCSS at each position, the color code is our own, and PI
// is clear, as in ipsc2hbpc.
func (t *IPSCTranslator) populateEmbeddedSignalling(burstIdx int) (pdu.EMBInfo, error) {
	lcss, _ := voiceBurst(burstIdx).LCSS()
	emb := pdu.EmbeddedSignalling{ColorCode: t.colorCode, LCSS: lcss}
	return emb.Info()
}
