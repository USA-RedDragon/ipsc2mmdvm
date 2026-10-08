// Package timeslot provides per-timeslot call arbitration for DMR.
//
// When multiple sources (MMDVM masters or IPSC peers) attempt to send
// audio on the same timeslot simultaneously, the first call is delivered
// immediately while subsequent calls are buffered in memory. When the
// active call terminates (or times out), buffered calls are delivered
// in FIFO order.
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

// activeCall tracks a single in-progress call on one timeslot.
type activeCall struct {
	streamID uint
	network  string    // human-readable label of the source (for logging)
	lastSeen time.Time // last time a packet was received for this call
}

// pendingStream holds buffered packets for a call waiting behind the
// active call on the same timeslot.
type pendingStream struct {
	streamID uint
	network  string
	packets  []any
}

// slotState tracks the active call and any pending calls on one timeslot.
type slotState struct {
	active  *activeCall
	pending []*pendingStream // FIFO queue of waiting calls
}

// Manager arbitrates access to DMR timeslots. Two timeslots exist
// (TS1 = Slot false, TS2 = Slot true). At most one call may be active
// on each slot at a time. Late-arriving calls on a busy slot are
// buffered and delivered when the active call ends.
//
// Create one Manager per traffic direction that needs isolation.
type Manager struct {
	mu        sync.Mutex
	slots     [2]*slotState // [0] = TS1 (Slot=false), [1] = TS2 (Slot=true)
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

// getOrCreateSlot returns the slotState for idx, creating it if needed.
// Must be called with mu held.
func (m *Manager) getOrCreateSlot(idx int) *slotState {
	if m.slots[idx] == nil {
		m.slots[idx] = &slotState{}
	}
	return m.slots[idx]
}

// findPending returns the pendingStream for streamID, or nil.
func (ss *slotState) findPending(streamID uint) *pendingStream {
	for _, p := range ss.pending {
		if p.streamID == streamID {
			return p
		}
	}
	return nil
}

// Submit adds a packet to the timeslot manager. If the stream is active
// (owns the slot), returns true and the caller should process the packet
// immediately. If the slot is busy with another call, the packet is
// buffered in memory and returns false — the caller should not process it.
//
// When the active call has timed out, all pending streams are discarded
// (they're stale) and the new stream becomes active.
func (m *Manager) Submit(slot bool, streamID uint, network string, packet any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := slotIndex(slot)
	ss := m.getOrCreateSlot(idx)
	now := time.Now()

	if ss.active == nil {
		// Slot is free — claim it.
		ss.active = &activeCall{
			streamID: streamID,
			network:  network,
			lastSeen: now,
		}
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction).Set(1)
		}
		slog.Debug("timeslot acquired",
			"slot", slot, "streamID", streamID, "network", network)
		return true
	}

	if ss.active.streamID == streamID {
		// Same stream — update timestamp and allow.
		ss.active.lastSeen = now
		return true
	}

	// Different stream — check for timeout on the active call.
	if now.Sub(ss.active.lastSeen) > m.timeout {
		slog.Info("timeslot call timed out, reclaiming slot",
			"slot", slot,
			"oldStream", ss.active.streamID, "oldNetwork", ss.active.network,
			"newStream", streamID, "newNetwork", network,
			"pendingDiscarded", len(ss.pending))
		if m.metrics != nil {
			m.metrics.TimeslotTimeouts.WithLabelValues(slotLabel(slot), m.direction).Inc()
		}
		// Discard stale pending streams and the timed-out active call.
		ss.pending = nil
		ss.active = &activeCall{
			streamID: streamID,
			network:  network,
			lastSeen: now,
		}
		return true
	}

	// Slot is busy — buffer the packet in a pending stream.
	ps := ss.findPending(streamID)
	if ps == nil {
		ps = &pendingStream{
			streamID: streamID,
			network:  network,
		}
		ss.pending = append(ss.pending, ps)
		slog.Debug("timeslot busy, buffering new stream",
			"slot", slot, "activeStream", ss.active.streamID,
			"pendingStream", streamID, "network", network)
	}
	ps.packets = append(ps.packets, packet)
	if m.metrics != nil {
		m.metrics.TimeslotPacketsBuffered.WithLabelValues(slotLabel(slot), m.direction).Inc()
	}
	return false
}

// Release frees a timeslot if it is currently held by the given stream.
// If pending streams are queued, the first one becomes active and its
// buffered packets are returned for immediate delivery by the caller.
// Returns nil when no pending streams exist.
func (m *Manager) Release(slot bool, streamID uint) []any {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := slotIndex(slot)
	ss := m.slots[idx]
	if ss == nil || ss.active == nil || ss.active.streamID != streamID {
		return nil
	}

	slog.Debug("timeslot released",
		"slot", slot, "streamID", streamID, "network", ss.active.network,
		"pendingCount", len(ss.pending))

	if len(ss.pending) == 0 {
		// No pending streams — slot is free.
		ss.active = nil
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction).Set(0)
		}
		return nil
	}

	// Activate the first pending stream.
	next := ss.pending[0]
	ss.pending = ss.pending[1:]
	ss.active = &activeCall{
		streamID: next.streamID,
		network:  next.network,
		lastSeen: time.Now(),
	}
	slog.Debug("timeslot activating pending stream",
		"slot", slot, "streamID", next.streamID, "network", next.network,
		"bufferedPackets", len(next.packets))
	return next.packets
}
