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
// with the new source, destination, and call type. The feature set ID and
// service options of the original LC are kept unless the call type changes.
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
		if dataType != enums.DataTypeVoiceLCHeader && dataType != enums.DataTypeTerminatorWithLC {
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
