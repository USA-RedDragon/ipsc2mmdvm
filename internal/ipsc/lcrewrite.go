package ipsc

import (
	"log/slog"
	"sync"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	mmdvm "github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

// LCRewriter keeps the link control inside DMR bursts in step with the
// routing fields of a packet after a rewrite rule changed them, as
// DMRGateway's CRewrite::processMessage does. It re-encodes the full LC of
// voice LC headers and terminators and the embedded LC of voice bursts B-E
// with the new source, destination, and call type. The service options of
// the original LC are kept. Its feature set ID is kept unless the call type
// changes, which resets it to the standard one. CSBKs and data headers that
// carry a target and a source address get the new addresses with a fresh
// CRC, and the new group flag where their format has one: the preamble,
// P_CLEAR, P_PROTECT, and AHOY CSBKs and every data header format except a
// response header.
//
// A rewritten stream keeps state between its voice header and its
// terminator, so one LCRewriter must see one direction of traffic only.
type LCRewriter struct {
	mu        sync.Mutex
	streams   map[uint]*lcStream
	now       func() time.Time
	lastPrune time.Time
}

type lcStream struct {
	lc       [9]byte
	embedded [4][4]byte
	lastSeen time.Time

	// Embedded LCs received in the current and the previous superframe,
	// as in DMRGateway's CRewrite m_data, m_writeNum, m_readNum, m_lastN.
	received [2]embeddedLCBuffer
	writeNum int
	readNum  int
	lastPos  int
}

// embeddedLCBuffer collects the four fragments of one received embedded LC,
// as DMRGateway's CDMREmbeddedData::addData does.
type embeddedLCBuffer struct {
	raw   [4][4]byte
	next  int // fragments collected so far in this superframe
	valid bool
	flco  enums.FLCO
}

func (b *embeddedLCBuffer) add(frag [4]byte, lcss enums.LCSS) {
	switch {
	case lcss == enums.LCSSFirstFragment:
		b.next, b.valid = 0, false
	case lcss == enums.LCSSContinuationFragment && (b.next == 1 || b.next == 2):
	case lcss == enums.LCSSLastFragment && b.next == 3:
	default:
		return
	}
	b.raw[b.next] = frag
	b.next++
	if b.next < 4 {
		return
	}
	b.next = 0
	lc, result := layer2.DecodeEmbeddedLC(b.raw, true)
	if result.Uncorrectable {
		return
	}
	b.valid = true
	b.flco = pdu.LCBytes(lc).FLCO()
	b.raw = layer2.EncodeEmbeddedLC(lc)
}

// swap moves to the next superframe, as DMRGateway's CRewrite::swap does
// (Rewrite.cpp lines 125-145).
func (st *lcStream) swap() {
	if st.lastPos == 0 {
		return
	}
	if st.readNum == 0 && st.writeNum == 0 {
		st.writeNum = 1
		return
	}
	st.readNum ^= 1
	st.writeNum ^= 1
}

// processEmbedded records the embedded LC fragment of voice burst pos (B-E)
// and replaces it, as DMRGateway's CRewrite::processEmbeddedData does
// (Rewrite.cpp lines 97-123). The decision for a superframe uses the
// embedded LC of the previous one, since a fragment must be sent before the
// LC it belongs to is complete: a voice LC (group or unit-to-unit) is
// replaced by the rewritten LC, and any other LC, such as a talker alias or
// GPS, passes through one superframe late. The first superframe always
// carries the rewritten LC. Unlike DMRGateway, an embedded LC that failed to
// decode is replaced rather than passed through.
func (st *lcStream) processEmbedded(data *[33]byte, pos int) {
	raw := layer2.RawBurst(*data)
	emb := raw.EMB()
	st.received[st.writeNum].add(raw.EmbeddedData(), emb.LCSS)

	out := st.embedded
	if st.readNum != 0 || st.writeNum != 0 {
		prev := &st.received[st.readNum]
		voice := prev.flco == enums.FLCOGroupVoiceChannelUser || prev.flco == enums.FLCOUnitToUnitVoiceChannelUser
		if prev.valid && !voice {
			out = prev.raw
		}
	}
	putEmbeddedFragment(data, out, pos)
}

// NewLCRewriter returns an empty LCRewriter.
func NewLCRewriter() *LCRewriter {
	return &LCRewriter{streams: make(map[uint]*lcStream), now: time.Now}
}

// Rewrite updates pkt.DMRData to match pkt when a rewrite rule changed the
// source, destination, or call type of orig. Other packets are not changed.
func (r *LCRewriter) Rewrite(orig mmdvm.Packet, pkt *mmdvm.Packet) {
	if orig.Src == pkt.Src && orig.Dst == pkt.Dst && orig.GroupCall == pkt.GroupCall {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)

	switch pkt.FrameType {
	case mmdvmFrameTypeDataSync:
		dataType := enums.DataType(pkt.DTypeOrVSeq) //nolint:gosec // compared against known data types only
		switch dataType {
		case enums.DataTypeCSBK:
			rewriteDataBlock(pkt, dataType, rewriteCSBK)
			return
		case enums.DataTypeDataHeader:
			rewriteDataBlock(pkt, dataType, rewriteDataHeader)
			return
		case enums.DataTypeVoiceLCHeader, enums.DataTypeTerminatorWithLC:
		case enums.DataTypePIHeader, enums.DataTypeMBCHeader, enums.DataTypeMBCContinuation,
			enums.DataTypeRate12, enums.DataTypeRate34, enums.DataTypeRate1,
			enums.DataTypeIdle, enums.DataTypeUnifiedSingleBlock, enums.DataTypeReserved:
			return
		default:
			return
		}
		lc := rewrittenLC(pkt, dataType)
		(*layer2.RawBurst)(&pkt.DMRData).SetBPTCPayload(fullLCCodeword(lc, dataType))
		if dataType == enums.DataTypeTerminatorWithLC {
			delete(r.streams, pkt.StreamID)
			return
		}
		r.streams[pkt.StreamID] = &lcStream{lc: lc, embedded: layer2.EncodeEmbeddedLC(lc), lastSeen: now}
	case mmdvmFrameTypeVoiceSync:
		if st, ok := r.streams[pkt.StreamID]; ok {
			st.lastSeen = now
			st.swap()
			st.lastPos = 0
		}
	case mmdvmFrameTypeVoice:
		pos, ok := mmdvmVoiceBurstPosition(*pkt)
		if !ok || pos < burstB {
			return
		}
		st, ok := r.streams[pkt.StreamID]
		if !ok {
			st = &lcStream{lc: constructLC(pkt.Src, pkt.Dst, pkt.GroupCall)}
			st.embedded = layer2.EncodeEmbeddedLC(st.lc)
			r.streams[pkt.StreamID] = st
		}
		st.lastSeen = now
		if pos < st.lastPos {
			st.swap()
		}
		if pos <= burstE {
			st.processEmbedded(&pkt.DMRData, pos)
		}
		st.lastPos = pos
	}
}

func (r *LCRewriter) pruneLocked(now time.Time) {
	if now.Sub(r.lastPrune) < pruneInterval {
		return
	}
	r.lastPrune = now
	for id, st := range r.streams {
		if now.Sub(st.lastSeen) > streamIdleTimeout {
			delete(r.streams, id)
		}
	}
}

// rewriteDataBlock rewrites the 12-byte BPTC(196,96) block of a CSBK or data
// header of dataType with edit, which returns the new block with its masked
// CRC, as DMRGateway's CRewrite::processCSBK and processDataHeader do
// (Rewrite.cpp lines 189-206 and 213-230). Blocks that fail their CRC, or
// that edit refuses, are left alone.
func rewriteDataBlock(pkt *mmdvm.Packet, dataType enums.DataType, edit func(b [12]byte, pkt *mmdvm.Packet) ([12]byte, bool)) {
	b, ok := decodeBPTCPayload(pkt.DMRData)
	if !ok || !pdu.BlockCRCOK(b, dataType) {
		return
	}
	if b, ok = edit(b, pkt); !ok {
		return
	}
	(*layer2.RawBurst)(&pkt.DMRData).SetBPTCPayload(b)
}

// rewriteCSBK sets the addresses of a CSBK, as DMRGateway's
// CDMRCSBK::setSrcId and setDstId do, and the group flag of every CSBK that
// has one: the preamble, P_CLEAR, P_PROTECT, and AHOY, where DMRGateway's
// setGI sets it on the preamble only. It refuses a CSBK whose layout does
// not carry both a target and a source address, such as C_ALOHA or a
// manufacturer CSBK, where DMRGateway writes octets 4-9 anyway.
func rewriteCSBK(b [12]byte, pkt *mmdvm.Packet) ([12]byte, bool) {
	csbk := pdu.CSBKBytes(b)
	if !csbk.SetAddresses(lcAddress(pkt.Dst), lcAddress(pkt.Src)) {
		return b, false
	}
	csbk.SetGroup(pkt.GroupCall)
	csbk.Reseal()
	return csbk, true
}

// rewriteDataHeader sets the group flag and addresses of a data header as
// DMRGateway's CDMRDataHeader does. A response header has no group flag, so
// its reserved bit stays as it is where DMRGateway writes the flag.
// Proprietary headers and reserved formats, which carry no addresses, are
// not changed.
func rewriteDataHeader(b [12]byte, pkt *mmdvm.Packet) ([12]byte, bool) {
	h := pdu.DataHeaderBytes(b)
	if !h.SetAddresses(lcAddress(pkt.Dst), lcAddress(pkt.Src)) {
		return b, false
	}
	h.SetGroup(pkt.GroupCall)
	h.Reseal()
	return h, true
}

// rewrittenLC returns the 9-byte LC for pkt. It starts from the valid LC
// in the burst, or from one built from the packet fields, and sets the
// call type, destination, and source from pkt. The protect flag stays as
// it is. A call type change clears the reserved bit, as DMRGateway does.
func rewrittenLC(pkt *mmdvm.Packet, dataType enums.DataType) pdu.LCBytes {
	var lc pdu.LCBytes
	payload, ok := decodeBPTCPayload(pkt.DMRData)
	if ok && layer2.MMDVMHostAccepts(dataType, payload) {
		lc = pdu.LCBytes(payload[:9])
	} else {
		slog.Debug("invalid full LC in burst, constructing from packet fields",
			"dtype", dataType.String())
		lc = constructLC(pkt.Src, pkt.Dst, pkt.GroupCall)
	}

	flco := enums.FLCOUnitToUnitVoiceChannelUser
	if pkt.GroupCall {
		flco = enums.FLCOGroupVoiceChannelUser
	}
	if lc.FLCO() != flco {
		lc.SetFLCO(flco)
		lc.SetReserved(false)
		lc.SetFID(enums.FIDStandardized)
	}
	lc.SetAddresses(lcAddress(pkt.Dst), lcAddress(pkt.Src))
	return lc
}

// putEmbeddedFragment writes the embedded LC fragment for burst position pos
// (B-E) into a voice burst and makes its EMB carry the matching LCSS.
func putEmbeddedFragment(data *[33]byte, embedded [4][4]byte, pos int) {
	raw := (*layer2.RawBurst)(data)
	raw.SetEmbeddedData(embedded[pos-burstB])

	emb := raw.EMB()
	if !emb.FEC.Uncorrectable && emb.LCSS != defaultLCSS(pos) {
		emb.LCSS = defaultLCSS(pos)
		raw.SetEMB(emb) // a decoded EMB always encodes
	}
}
