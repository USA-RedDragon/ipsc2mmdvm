package mmdvm

import (
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

type openDataCall struct {
	last  dmrd.Data
	seen  time.Time
	timer *time.Timer
}

func (h *MMDVMClient) followDataCall(pkt dmrd.Data, end bool) {
	if pkt.Frame != dmrd.FrameDataSync || OutboundPacketKind(pkt) == timeslot.KindVoice ||
		enums.DataType(pkt.Value) == enums.DataTypeCSBK && isPreambleCSBK(pkt.Burst) { //nolint:gosec // the data type has 4 bits
		return
	}
	h.openDataMu.Lock()
	defer h.openDataMu.Unlock()
	oc := h.openData[pkt.Stream]
	if end {
		if oc != nil {
			oc.timer.Stop()
			delete(h.openData, pkt.Stream)
		}
		return
	}
	select {
	case <-h.done:
		return
	default:
	}
	if oc == nil {
		if h.openData == nil {
			h.openData = make(map[uint32]*openDataCall)
		}
		oc = &openDataCall{}
		h.openData[pkt.Stream] = oc
		stream := pkt.Stream
		oc.timer = time.AfterFunc(preambleGap, func() { h.dataCallQuiet(stream, oc) })
	} else {
		oc.timer.Reset(preambleGap)
	}
	oc.last, oc.seen = pkt, time.Now()
}

func (h *MMDVMClient) dataCallQuiet(stream uint32, oc *openDataCall) {
	h.openDataMu.Lock()
	if h.openData[stream] != oc {
		h.openDataMu.Unlock()
		return
	}
	if left := preambleGap - time.Since(oc.seen); left > 0 {
		oc.timer.Reset(left)
		h.openDataMu.Unlock()
		return
	}
	delete(h.openData, stream)
	last := oc.last
	h.openDataMu.Unlock()
	h.endOutboundCall(&preambleCall{id: stream, last: last})
}

func (h *MMDVMClient) forgetDataCalls() {
	h.openDataMu.Lock()
	defer h.openDataMu.Unlock()
	for id, oc := range h.openData {
		oc.timer.Stop()
		delete(h.openData, id)
	}
}
