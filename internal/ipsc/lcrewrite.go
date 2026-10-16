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

	// colorCode is the color code of the voice header's slot type, or of
	// the first correctable EMB of a late entry.
	colorCode enums.ColorCode

	// Embedded LCs received in the current and the previous superframe,
	// as in DMRGateway's CRewrite m_data, m_writeNum, m_readNum, m_lastN.
	received [2]embeddedLCBuffer
	writeNum int
	readNum  int
	lastPos  int
}

// embeddedFragments collects the four embedded LC fragments of voice bursts
// B-E of one superframe, so the four always come from the same superframe.
// Burst B starts a new collection. Any other break drops the fragments
// collected so far: a burst out of order or after a lost burst, or an EMB
// that does not decode or has the wrong LCSS for the burst. MMDVMHost's
// CDMREmbeddedData::addData collects fragments by their LCSS, and MMDVMHost
// empties the buffer at each voice sync (DMRSlot.cpp lines 664-667).
// Dropping the fragments on a lost or out-of-order burst is stricter.
type embeddedFragments struct {
	raw  [4][4]byte
	next int // fragments collected so far
}

// add adds frag, the embedded fragment of the voice burst at pos (B-E) with
// EMB emb, which decoded when embOK is set. lastPos is the position of the
// burst before it. add reports whether frag completes the four fragments.
func (f *embeddedFragments) add(frag [4]byte, emb pdu.EmbeddedSignalling, embOK bool, pos, lastPos int) bool {
	if pos == burstB {
		f.next = 0
	}
	lcss, _ := voiceBurst(pos).LCSS()
	if !embOK || emb.LCSS != lcss || pos-burstB != f.next ||
		(pos != burstB && pos != lastPos+1) {
		f.next = 0
		return false
	}
	f.raw[f.next] = frag
	f.next++
	if f.next < 4 {
		return false
	}
	f.next = 0
	return true
}

// embeddedLCBuffer holds the embedded LC received in one superframe, as
// DMRGateway's CDMREmbeddedData does.
type embeddedLCBuffer struct {
	fragments embeddedFragments
	raw       [4][4]byte // the LC re-encoded, when valid
	valid     bool
	flco      enums.FLCO
}

// reset empties the buffer for a new superframe, as MMDVMHost's
// CDMREmbeddedData::reset does.
func (b *embeddedLCBuffer) reset() {
	b.fragments.next = 0
	b.valid = false
}

func (b *embeddedLCBuffer) add(frag [4]byte, emb pdu.EmbeddedSignalling, embOK bool, pos, lastPos int) {
	if !b.fragments.add(frag, emb, embOK, pos, lastPos) {
		return
	}
	lc, result := layer2.DecodeEmbeddedLC(b.fragments.raw, true)
	if result.Uncorrectable {
		return
	}
	b.valid = true
	b.flco = pdu.LCBytes(lc).FLCO()
	b.raw = layer2.EncodeEmbeddedLC(lc)
}

// swap moves to the next superframe, as DMRGateway's CRewrite::swap does
// (Rewrite.cpp lines 125-145), and empties the buffer for it, as
// MMDVMHost's CDMRSlot::writeModem does, so no fragment of an older
// superframe is left in it.
func (st *lcStream) swap() {
	if st.lastPos == 0 {
		return
	}
	if st.readNum == 0 && st.writeNum == 0 {
		st.writeNum = 1
	} else {
		st.readNum ^= 1
		st.writeNum ^= 1
	}
	st.received[st.writeNum].reset()
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
	st.received[st.writeNum].add(raw.EmbeddedData(), emb, !emb.FEC.Uncorrectable, pos, st.lastPos)

	out := st.embedded
	if st.readNum != 0 || st.writeNum != 0 {
		prev := &st.received[st.readNum]
		voice := prev.flco == enums.FLCOGroupVoiceChannelUser || prev.flco == enums.FLCOUnitToUnitVoiceChannelUser
		if prev.valid && !voice {
			out = prev.raw
		}
	}
	putEmbeddedFragment(data, out, pos, st.colorCode)
}

// NewLCRewriter returns an empty LCRewriter.
func NewLCRewriter() *LCRewriter {
	return &LCRewriter{streams: make(map[uint]*lcStream), now: time.Now}
}

// Rewrite updates pkt.DMRData to match pkt when a rewrite rule changed the
// source, destination, or call type of orig. Other packets are not changed.
func (r *LCRewriter) Rewrite(orig mmdvm.Packet, pkt *mmdvm.Packet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	if orig.Src == pkt.Src && orig.Dst == pkt.Dst && orig.GroupCall == pkt.GroupCall {
		return
	}

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
		r.streams[pkt.StreamID] = &lcStream{lc: lc, embedded: layer2.EncodeEmbeddedLC(lc), lastSeen: now, colorCode: slotTypeColorCode(pkt.DMRData)}
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
			if emb := layer2.RawBurst(pkt.DMRData).EMB(); !emb.FEC.Uncorrectable {
				st.colorCode = emb.ColorCode
			}
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
	b, ok := decodeBPTCPayload(pkt.DMRData, dataType)
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
	if payload, ok := decodeBPTCPayload(pkt.DMRData, dataType); ok {
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
// (B-E) into a voice burst and gives it a new EMB with colorCode, the
// matching LCSS, and PI clear. The burst's own EMB may be corrupt, so it
// is not used.
func putEmbeddedFragment(data *[33]byte, embedded [4][4]byte, pos int, colorCode enums.ColorCode) {
	raw := (*layer2.RawBurst)(data)
	raw.SetEmbeddedData(embedded[pos-burstB])
	lcss, _ := voiceBurst(pos).LCSS()
	raw.SetEMB(pdu.EmbeddedSignalling{ColorCode: colorCode, LCSS: lcss})
}
