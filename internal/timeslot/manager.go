// Package timeslot provides per-timeslot call arbitration for DMR.
//
// When multiple sources (MMDVM masters or IPSC peers) attempt to send
// audio on the same timeslot simultaneously, the first call is delivered
// immediately while subsequent calls are buffered in memory. When the
// active call terminates (or times out), buffered calls are delivered
// in FIFO order through the sink of the source that submitted them.
package timeslot

import (
	"log/slog"
	"sync"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
)

// DefaultTimeout is the duration after which an active call is considered
// stale and its timeslot can be reclaimed. This covers the case where
// a voice terminator packet is lost.
const DefaultTimeout = 3 * time.Second

// Verdict tells the caller of Submit what to do with a packet.
type Verdict int

const (
	// Deliver means the stream owns the slot and the caller must
	// deliver the packet now.
	Deliver Verdict = iota
	// Queued means the manager holds the packet and delivers it later
	// through the stream's sink.
	Queued
)

// Sink delivers one buffered packet on behalf of the source that
// submitted it.
type Sink func(packet any)

type entry struct {
	packet any
	end    bool
}

// stream is one call on a timeslot, either active or waiting.
type stream struct {
	id       uint
	network  string
	sink     Sink
	queue    []entry
	lastSeen time.Time
}

// slotState tracks the active call and any pending calls on one timeslot.
type slotState struct {
	active *stream
	// draining is true while the active stream still has queued packets
	// that must be delivered before its live packets.
	draining bool
	pending  []*stream
}

// Manager arbitrates access to DMR timeslots. Two timeslots exist
// (TS1 = Slot false, TS2 = Slot true). At most one call may be active
// on each slot at a time. Late-arriving calls on a busy slot are
// buffered and delivered when the active call ends.
//
// Create one Manager per traffic direction that needs isolation.
type Manager struct {
	mu        sync.Mutex
	slots     [2]slotState // [0] = TS1 (Slot=false), [1] = TS2 (Slot=true)
	timeout   time.Duration
	metrics   *metrics.Metrics
	direction string // "inbound" or "outbound" (for metric labels)
}

// NewManager creates a Manager with the default timeout.
func NewManager() *Manager {
	return &Manager{
		timeout: DefaultTimeout,
	}
}

// SetMetrics configures the metrics collector and direction label for this manager.
func (m *Manager) SetMetrics(met *metrics.Metrics, direction string) {
	m.metrics = met
	m.direction = direction
}

// slotIndex converts the boolean Slot flag to an array index.
func slotIndex(slot bool) int {
	if slot {
		return 1
	}
	return 0
}

// slotLabel returns a string label for metrics ("1" or "2").
func slotLabel(slot bool) string {
	if slot {
		return "2"
	}
	return "1"
}

// findPending returns the pending stream for streamID, or nil.
func (ss *slotState) findPending(streamID uint) *stream {
	for _, p := range ss.pending {
		if p.id == streamID {
			return p
		}
	}
	return nil
}

// Submit offers a packet from streamID to the timeslot. end marks the
// packet that ends the call. sink delivers the packet later if the
// manager buffers it, so it must send through the same path that the
// caller uses for packets it delivers itself.
//
// On Deliver the caller must deliver the packet, and when end is true
// it must call Release after the delivery. On Queued the manager owns
// the packet.
//
// When the active call has timed out, all pending streams are discarded
// (they're stale) and the new stream becomes active.
func (m *Manager) Submit(slot bool, streamID uint, network string, packet any, end bool, sink Sink) Verdict {
	m.mu.Lock()
	defer m.mu.Unlock()

	ss := &m.slots[slotIndex(slot)]
	now := time.Now()

	if ss.active == nil {
		ss.active = &stream{id: streamID, network: network, sink: sink, lastSeen: now}
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction).Set(1)
		}
		slog.Debug("timeslot acquired",
			"slot", slot, "streamID", streamID, "network", network)
		return Deliver
	}

	if ss.active.id == streamID {
		ss.active.lastSeen = now
		if !ss.draining {
			return Deliver
		}
		m.enqueueLocked(slot, ss.active, packet, end)
		return Queued
	}

	if now.Sub(ss.active.lastSeen) > m.timeout {
		slog.Info("timeslot call timed out, reclaiming slot",
			"slot", slot,
			"oldStream", ss.active.id, "oldNetwork", ss.active.network,
			"newStream", streamID, "newNetwork", network,
			"pendingDiscarded", len(ss.pending))
		if m.metrics != nil {
			m.metrics.TimeslotTimeouts.WithLabelValues(slotLabel(slot), m.direction).Inc()
		}
		ss.pending = nil
		ss.draining = false
		ss.active = &stream{id: streamID, network: network, sink: sink, lastSeen: now}
		return Deliver
	}

	ps := ss.findPending(streamID)
	if ps == nil {
		ps = &stream{id: streamID, network: network, sink: sink}
		ss.pending = append(ss.pending, ps)
		slog.Debug("timeslot busy, buffering new stream",
			"slot", slot, "activeStream", ss.active.id,
			"pendingStream", streamID, "network", network)
	}
	ps.lastSeen = now
	m.enqueueLocked(slot, ps, packet, end)
	return Queued
}

func (m *Manager) enqueueLocked(slot bool, s *stream, packet any, end bool) {
	s.queue = append(s.queue, entry{packet: packet, end: end})
	if m.metrics != nil {
		m.metrics.TimeslotPacketsBuffered.WithLabelValues(slotLabel(slot), m.direction).Inc()
	}
}

// Release frees a timeslot if it is currently held by the given stream.
// If calls are waiting, the oldest one becomes active and its buffered
// packets are delivered through its own sink, followed by any later
// calls that are complete in the buffer.
func (m *Manager) Release(slot bool, streamID uint) {
	m.mu.Lock()
	ss := &m.slots[slotIndex(slot)]
	if ss.active == nil || ss.active.id != streamID || ss.draining {
		m.mu.Unlock()
		return
	}
	slog.Debug("timeslot released",
		"slot", slot, "streamID", streamID, "network", ss.active.network,
		"pendingCount", len(ss.pending))
	replay := m.advanceLocked(slot, ss)
	m.mu.Unlock()
	if replay {
		m.replay(slot)
	}
}

// advanceLocked ends the active call and promotes the oldest pending
// call. It returns true when the promoted call has packets to replay.
func (m *Manager) advanceLocked(slot bool, ss *slotState) bool {
	ss.draining = false
	if len(ss.pending) == 0 {
		ss.active = nil
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction).Set(0)
		}
		return false
	}
	next := ss.pending[0]
	ss.pending[0] = nil
	ss.pending = ss.pending[1:]
	next.lastSeen = time.Now()
	ss.active = next
	slog.Debug("timeslot activating pending stream",
		"slot", slot, "streamID", next.id, "network", next.network,
		"bufferedPackets", len(next.queue))
	ss.draining = len(next.queue) > 0
	return ss.draining
}

// replay delivers the active stream's queued packets in order. Packets
// that arrive for the stream during the replay join its queue, so they
// are delivered after the older ones. When a queued packet ends the
// call, the next pending call is promoted and replayed in turn.
func (m *Manager) replay(slot bool) {
	ss := &m.slots[slotIndex(slot)]
	for {
		m.mu.Lock()
		s := ss.active
		if s == nil || !ss.draining || len(s.queue) == 0 {
			ss.draining = false
			m.mu.Unlock()
			return
		}
		e := s.queue[0]
		s.queue[0] = entry{}
		s.queue = s.queue[1:]
		s.lastSeen = time.Now()
		m.mu.Unlock()

		if s.sink != nil {
			s.sink(e.packet)
		}

		if e.end {
			m.mu.Lock()
			if ss.active == s {
				m.advanceLocked(slot, ss)
			}
			m.mu.Unlock()
		}
	}
}
