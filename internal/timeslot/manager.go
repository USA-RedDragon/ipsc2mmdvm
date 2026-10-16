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

// AllNetworks is the network label of a manager shared by all networks.
const AllNetworks = "all"

// DefaultTimeout is the duration after which an active call is considered
// stale and its timeslot can be reclaimed. This covers the case where
// a voice terminator packet is lost.
const DefaultTimeout = 3 * time.Second

const (
	// MaxPendingCalls is the number of calls that may wait behind the
	// active call on one timeslot.
	MaxPendingCalls = 8
	// MaxQueuedPackets is the number of packets buffered for one call,
	// about 30 seconds of voice at 16.7 packets per second. A call's
	// terminator is still buffered when the limit is reached.
	MaxQueuedPackets = 500
	// MaxPendingAge is how long a complete call may wait for the slot
	// after its last packet before it is discarded.
	MaxPendingAge = 30 * time.Second
)

// Verdict tells the caller of Submit what to do with a packet.
type Verdict int

const (
	// Deliver means the stream owns the slot and the caller must
	// deliver the packet now.
	Deliver Verdict = iota
	// Queued means the manager holds the packet and delivers it later
	// through the stream's sink.
	Queued
	// Dropped means the manager discarded the packet because a buffer
	// limit was reached or the manager is stopped.
	Dropped
)

const (
	dropPendingFull = "pending_full"
	dropQueueFull   = "queue_full"
	dropExpired     = "expired"
	dropAfterEnd    = "after_end"
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
	ended    bool // the call's terminator is in queue
	lastSeen time.Time
}

// expired reports whether a waiting call should be discarded. A call
// that stopped sending without a terminator expires after timeout. A
// complete call may wait up to MaxPendingAge.
func (s *stream) expired(now time.Time, timeout time.Duration) bool {
	return now.Sub(s.lastSeen) > s.maxIdle(timeout)
}

func (s *stream) maxIdle(timeout time.Duration) time.Duration {
	if s.ended {
		return MaxPendingAge
	}
	return timeout
}

// slotState tracks the active call and any pending calls on one timeslot.
type slotState struct {
	active *stream
	// draining is true while the active stream still has queued packets
	// that must be delivered before its live packets.
	draining bool
	// sending is true while the slot's sender goroutine runs.
	sending bool
	pending []*stream // waiting calls in arrival order
	byID    map[uint]*stream
	// timer reclaims the slot when the active call goes stale, so
	// waiting calls do not depend on a new packet to be promoted.
	timer *time.Timer
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
	network   string // network name, or "all" for a shared manager (for metric labels)
	stopped   bool
	wg        sync.WaitGroup
	// now returns the current time. Tests replace it to control timeouts.
	now func() time.Time
}

// NewManager creates a Manager with the default timeout.
func NewManager() *Manager {
	return &Manager{
		timeout: DefaultTimeout,
		now:     time.Now,
	}
}

// SetMetrics configures the metrics collector and the direction and
// network labels for this manager.
func (m *Manager) SetMetrics(met *metrics.Metrics, direction, network string) {
	m.metrics = met
	m.direction = direction
	m.network = network
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

// Submit offers a packet from streamID to the timeslot. end marks the
// packet that ends the call. sink delivers the packet later if the
// manager buffers it, so it must send through the same path that the
// caller uses for packets it delivers itself.
//
// On Deliver the caller must deliver the packet, and when end is true
// it must call Release after the delivery. On Queued the manager owns
// the packet. On Dropped the packet is discarded.
//
// When the active call has timed out, the oldest pending call takes over
// the slot with its buffered packets, or the submitting stream claims the
// slot if no call is waiting.
func (m *Manager) Submit(slot bool, streamID uint, network string, packet any, end bool, sink Sink) Verdict {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return Dropped
	}

	ss := &m.slots[slotIndex(slot)]
	now := m.now()

	if ss.active == nil || ss.active.id != streamID {
		m.sweepLocked(slot, ss, now)
	}

	if ss.active == nil {
		ss.active = &stream{id: streamID, network: network, sink: sink, lastSeen: now}
		m.armLocked(slot, ss, now)
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction, m.network).Set(1)
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
		return m.enqueueLocked(slot, ss.active, packet, end)
	}

	ps := ss.byID[streamID]
	if ps == nil {
		if len(ss.pending) >= MaxPendingCalls {
			m.dropLocked(slot, dropPendingFull, 1)
			return Dropped
		}
		ps = &stream{id: streamID, network: network, sink: sink}
		ss.pending = append(ss.pending, ps)
		if ss.byID == nil {
			ss.byID = make(map[uint]*stream)
		}
		ss.byID[streamID] = ps
		slog.Debug("timeslot busy, buffering new stream",
			"slot", slot, "activeStream", ss.active.id,
			"pendingStream", streamID, "network", network)
	}
	ps.lastSeen = now
	return m.enqueueLocked(slot, ps, packet, end)
}

// expirePendingLocked discards waiting calls that went idle.
func (m *Manager) expirePendingLocked(slot bool, ss *slotState, now time.Time) {
	kept := ss.pending[:0]
	for _, p := range ss.pending {
		if !p.expired(now, m.timeout) {
			kept = append(kept, p)
			continue
		}
		slog.Info("timeslot discarding idle pending stream",
			"slot", slot, "streamID", p.id, "network", p.network,
			"bufferedPackets", len(p.queue))
		delete(ss.byID, p.id)
		m.dropLocked(slot, dropExpired, len(p.queue))
	}
	clear(ss.pending[len(kept):])
	ss.pending = kept
}

func (m *Manager) dropLocked(slot bool, reason string, n int) {
	if m.metrics != nil && n > 0 {
		m.metrics.TimeslotPacketsDropped.WithLabelValues(slotLabel(slot), m.direction, m.network, reason).Add(float64(n))
	}
}

// reclaimStaleLocked ends the active call if no packet has arrived for
// it within the timeout, which happens when its terminator is lost.
// The oldest pending call takes over the slot with its buffer intact.
func (m *Manager) reclaimStaleLocked(slot bool, ss *slotState, now time.Time) {
	if ss.active == nil || ss.draining || now.Sub(ss.active.lastSeen) <= m.timeout {
		return
	}
	slog.Info("timeslot call timed out, reclaiming slot",
		"slot", slot,
		"oldStream", ss.active.id, "oldNetwork", ss.active.network,
		"pendingCount", len(ss.pending))
	if m.metrics != nil {
		m.metrics.TimeslotTimeouts.WithLabelValues(slotLabel(slot), m.direction, m.network).Inc()
	}
	m.advanceLocked(slot, ss)
}

// armLocked schedules the next stale check on slot: when the active call
// or the first waiting call can expire.
func (m *Manager) armLocked(slot bool, ss *slotState, now time.Time) {
	if m.stopped || ss.active == nil {
		return
	}
	d := m.timeout
	if !ss.draining {
		d = ss.active.lastSeen.Add(m.timeout).Sub(now)
	}
	for _, p := range ss.pending {
		d = min(d, p.lastSeen.Add(p.maxIdle(m.timeout)).Sub(now))
	}
	d = max(d, 0) + time.Millisecond
	if ss.timer == nil {
		ss.timer = time.AfterFunc(d, func() { m.onTimer(slot) })
		return
	}
	ss.timer.Reset(d)
}

func (m *Manager) onTimer(slot bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	ss := &m.slots[slotIndex(slot)]
	now := m.now()
	m.sweepLocked(slot, ss, now)
	m.armLocked(slot, ss, now)
}

// sweepLocked applies every timeout that has passed by now, in the order
// the deadlines fell, so a late check gives the same result as a timely
// one: waiting calls that went idle before the active call went stale
// are discarded first, then the oldest remaining call takes over.
func (m *Manager) sweepLocked(slot bool, ss *slotState, now time.Time) {
	if a := ss.active; a != nil && !ss.draining {
		if staleAt := a.lastSeen.Add(m.timeout); now.After(staleAt) {
			m.expirePendingLocked(slot, ss, staleAt)
			m.reclaimStaleLocked(slot, ss, now)
		}
	}
	m.expirePendingLocked(slot, ss, now)
}

func (m *Manager) enqueueLocked(slot bool, s *stream, packet any, end bool) Verdict {
	if s.ended {
		m.dropLocked(slot, dropAfterEnd, 1)
		return Dropped
	}
	if len(s.queue) >= MaxQueuedPackets && !end {
		m.dropLocked(slot, dropQueueFull, 1)
		return Dropped
	}
	s.queue = append(s.queue, entry{packet: packet, end: end})
	if end {
		s.ended = true
	}
	if m.metrics != nil {
		m.metrics.TimeslotPacketsBuffered.WithLabelValues(slotLabel(slot), m.direction, m.network).Inc()
	}
	return Queued
}

// Release frees a timeslot if it is currently held by the given stream.
// If calls are waiting, the oldest one becomes active. Its buffered
// packets, and those of later calls that are complete in the buffer,
// are delivered in order through their own sinks by a per-slot sender
// goroutine, so Release does not wait for the replay.
func (m *Manager) Release(slot bool, streamID uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss := &m.slots[slotIndex(slot)]
	if m.stopped || ss.active == nil || ss.active.id != streamID || ss.draining {
		return
	}
	slog.Debug("timeslot released",
		"slot", slot, "streamID", streamID, "network", ss.active.network,
		"pendingCount", len(ss.pending))
	m.advanceLocked(slot, ss)
}

// Stop halts replays and stale-call timers and waits for any delivery in
// progress to return. Packets still buffered are discarded and later
// Submit and Release calls do nothing.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	for i := range m.slots {
		if t := m.slots[i].timer; t != nil {
			t.Stop()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// advanceLocked ends the active call, promotes the oldest pending call,
// and starts the slot's sender if the promoted call has packets queued.
func (m *Manager) advanceLocked(slot bool, ss *slotState) {
	ss.draining = false
	if len(ss.pending) == 0 {
		ss.active = nil
		if ss.timer != nil {
			ss.timer.Stop()
		}
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction, m.network).Set(0)
		}
		return
	}
	next := ss.pending[0]
	ss.pending[0] = nil
	ss.pending = ss.pending[1:]
	delete(ss.byID, next.id)
	now := m.now()
	next.lastSeen = now
	ss.active = next
	slog.Debug("timeslot activating pending stream",
		"slot", slot, "streamID", next.id, "network", next.network,
		"bufferedPackets", len(next.queue))
	ss.draining = len(next.queue) > 0
	m.armLocked(slot, ss, now)
	if ss.draining && !ss.sending && !m.stopped {
		ss.sending = true
		m.wg.Add(1)
		go m.send(slot)
	}
}

// send delivers the active stream's queued packets in order. Packets
// that arrive for the stream while it runs join its queue, so they are
// delivered after the older ones. When a queued packet ends the call,
// the next pending call is promoted and replayed in turn.
func (m *Manager) send(slot bool) {
	defer m.wg.Done()
	ss := &m.slots[slotIndex(slot)]
	for {
		m.mu.Lock()
		s := ss.active
		if m.stopped || s == nil || !ss.draining || len(s.queue) == 0 {
			ss.draining = false
			ss.sending = false
			m.mu.Unlock()
			return
		}
		e := s.queue[0]
		s.queue[0] = entry{}
		s.queue = s.queue[1:]
		s.lastSeen = m.now()
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
