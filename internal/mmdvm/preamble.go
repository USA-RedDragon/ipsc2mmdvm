package mmdvm

import (
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

// preambleGap is how long an outbound call waits for the burst that a
// preamble CSBK announces. An XPR 8400 sends the packets of a call with
// preambles at most 180 ms apart (cap/preamble.txt); twice that allows for
// network jitter.
const preambleGap = 360 * time.Millisecond

// preambleCall is an outbound call that started with a preamble CSBK on one
// timeslot.
type preambleCall struct {
	id      uint32          // the stream ID the call goes by
	joined  map[uint32]bool // the stream IDs of the bursts that joined it
	last    dmrd.Data       // the last packet of the call, with stream ID id
	seen    time.Time       // when last arrived
	waiting bool            // last is a preamble, so more of the call follows
	timer   *time.Timer
}

// outboundEnd ends an outbound call on IPSC when none of its bursts ends
// it: pkt is the last packet of the call.
type outboundEnd struct{ pkt dmrd.Data }

// joinPreambleCall returns pkt with the stream ID of the outbound call with
// preambles that it belongs to, and the call that pkt shows to be over,
// if any. MMDVMHost sends each CSBK and data header with a new stream ID,
// while an XPR 8400 sends the preamble CSBKs of a data call and the data
// that follows them as one call. A CSBK or data header joins the call
// before it on its timeslot when that call's last packet is a preamble
// CSBK less than preambleGap earlier, from the same source to the same
// destination. A data block keeps the stream ID of its header, so it joins
// when its header did. A packet of a new stream on the timeslot starts a
// call that competes for it, so it ends a call that waits for the burst
// its preamble announced. A packet of a stream that was on the timeslot
// already, such as a voice call that holds it while the call with
// preambles waits for it, leaves that call alone.
func (h *MMDVMClient) joinPreambleCall(pkt dmrd.Data) (dmrd.Data, *preambleCall) {
	h.preambleMu.Lock()
	defer h.preambleMu.Unlock()
	slot := slotIndex(pkt.Slot)
	pc := h.preambles[slot]
	now := time.Now()
	known := h.noteSlotStreamLocked(slot, pkt.Stream, now)
	dataType := enums.DataType(pkt.Value) //nolint:gosec // compared against known data types only
	dataSync := pkt.Frame == dmrd.FrameDataSync
	preamble := dataSync && dataType == enums.DataTypeCSBK && isPreambleCSBK(pkt.Burst)

	var ended *preambleCall
	switch {
	case pc != nil && (pkt.Stream == pc.id || pc.joined[pkt.Stream]):
	case pc != nil && pc.waiting && now.Sub(pc.seen) < preambleGap && dataSync &&
		(dataType == enums.DataTypeCSBK || dataType == enums.DataTypeDataHeader) &&
		pkt.Source == pc.last.Source && pkt.Destination == pc.last.Destination && pkt.Private == pc.last.Private:
		pc.joined[pkt.Stream] = true
	case pc != nil && pc.waiting && known && !preamble:
		return pkt, nil
	default:
		if pc != nil && (pc.waiting || preamble || now.Sub(pc.seen) > timeslot.DefaultTimeout) {
			h.forgetPreambleCallLocked(slot)
			if pc.waiting {
				ended = pc
			}
		}
		if !preamble {
			return pkt, ended
		}
		pc = &preambleCall{id: pkt.Stream, joined: make(map[uint32]bool)}
		h.preambles[slot] = pc
	}
	pkt.Stream = pc.id
	pc.last, pc.seen, pc.waiting = pkt, now, preamble
	switch {
	case !preamble && pc.timer != nil:
		pc.timer.Stop()
	case preamble && pc.timer == nil:
		pc.timer = time.AfterFunc(preambleGap, func() { h.preambleTimeout(slot, pc) })
	case preamble:
		pc.timer.Reset(preambleGap)
	}
	return pkt, ended
}

// noteSlotStreamLocked records that stream streamID sent on timeslot index
// slot at now, and reports whether it had sent on the timeslot in the
// timeslot.DefaultTimeout before. It forgets the streams that went quiet
// for longer. The caller must hold h.preambleMu.
func (h *MMDVMClient) noteSlotStreamLocked(slot int, streamID uint32, now time.Time) bool {
	streams := h.slotStreams[slot]
	if streams == nil {
		streams = make(map[uint32]time.Time)
		h.slotStreams[slot] = streams
	}
	for id, at := range streams {
		if now.Sub(at) > timeslot.DefaultTimeout {
			delete(streams, id)
		}
	}
	_, known := streams[streamID]
	streams[streamID] = now
	return known
}

// forgetPreambleCallLocked stops following the call with preambles on
// timeslot index slot. The caller must hold h.preambleMu.
func (h *MMDVMClient) forgetPreambleCallLocked(slot int) {
	if pc := h.preambles[slot]; pc != nil && pc.timer != nil {
		pc.timer.Stop()
	}
	h.preambles[slot] = nil
}

// preambleCallEnded stops following the call with preambles that pkt, a
// packet that ends its call, belongs to.
func (h *MMDVMClient) preambleCallEnded(pkt dmrd.Data) {
	h.preambleMu.Lock()
	defer h.preambleMu.Unlock()
	slot := slotIndex(pkt.Slot)
	if pc := h.preambles[slot]; pc != nil && pc.id == pkt.Stream {
		h.forgetPreambleCallLocked(slot)
	}
}

// preambleTimeout ends pc, the call with preambles on timeslot index slot,
// when the burst its last preamble announced has not come.
func (h *MMDVMClient) preambleTimeout(slot int, pc *preambleCall) {
	h.preambleMu.Lock()
	if h.preambles[slot] != pc || !pc.waiting {
		h.preambleMu.Unlock()
		return
	}
	if left := preambleGap - time.Since(pc.seen); left > 0 {
		pc.timer.Reset(left)
		h.preambleMu.Unlock()
		return
	}
	h.forgetPreambleCallLocked(slot)
	h.preambleMu.Unlock()
	h.endOutboundCall(pc)
}

// endOutboundCall ends pc on IPSC and on the outbound timeslot, after any
// of its packets that wait for the timeslot.
func (h *MMDVMClient) endOutboundCall(pc *preambleCall) {
	select {
	case <-h.done:
		return
	default:
	}
	end := outboundEnd{pc.last}
	if h.outboundTSMgr == nil {
		h.forwardOutbound(end)
		return
	}
	if h.outboundTSMgr.Submit(isTS2(pc.last.Slot), uint(pc.id), h.cfg.Name, end, true, h.forwardOutbound) == timeslot.Deliver {
		h.forwardOutbound(end)
		h.outboundTSMgr.Release(isTS2(pc.last.Slot), uint(pc.id))
	}
}

// slotIndex returns the index of a timeslot in per-slot arrays.
func slotIndex(slot enums.Slot) int {
	if slot == enums.Slot2 {
		return 1
	}
	return 0
}
