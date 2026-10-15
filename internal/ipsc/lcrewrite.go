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
	case mmdvmFrameTypeVoice:
		pos, ok := mmdvmVoiceBurstPosition(*pkt)
		if !ok || pos < burstB || pos > burstE {
			return
		}
		st, ok := r.streams[pkt.StreamID]
		if !ok {
			st = &lcStream{lc: constructLC(pkt.Src, pkt.Dst, pkt.GroupCall)}
			st.embedded = layer2.EncodeEmbeddedLC(st.lc)
			r.streams[pkt.StreamID] = st
		}
		st.lastSeen = now
		putEmbeddedFragment(&pkt.DMRData, st.embedded, pos)
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
