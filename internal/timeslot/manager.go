// Package timeslot provides per-timeslot call arbitration for DMR.
//
// When multiple sources (MMDVM masters or IPSC peers) attempt to send
// audio on the same timeslot simultaneously, the first call is delivered
// immediately while subsequent calls are buffered in memory. When the
// active call terminates (or times out), buffered calls are delivered
// in FIFO order through the sink of the source that submitted them.
package timeslot

import (
	"slices"
	"sync"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/logging"
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
	at     time.Time // when Submit got the packet
}

// stream is one call on a timeslot, either active or waiting.
type stream struct {
	id       uint
	network  string
	sink     Sink
	queue    []entry
	ended    bool // the call's terminator is in queue
	voice    bool // the first packet of the call is voice (SetCallHang)
	data     bool // a packet of the call is data (SetCallHang)
	lastSeen time.Time

	// The arrival time of the last packet the sender replayed, and when
	// the sender replayed it, so that it replays the next packet as far
	// after it as the packet arrived.
	replayedAt time.Time
	replayedOn time.Time
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
	// holdUntil is when the hold after the last voice or data call on
	// the slot ends (SetCallHang).
	holdUntil time.Time
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
	// now returns the current time and after waits as time.After does.
	// Tests replace them to control time.
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	stop  chan struct{} // closed by Stop
	// pace makes the sender replay a queued call at the spacing its
	// packets arrived with (PaceReplays).
	pace bool
	// logs holds the records made while mu is held.
	logs logging.Queue
	// hold, dataHold and kind are set by SetCallHang.
	hold     time.Duration
	dataHold time.Duration
	kind     func(packet any) PacketKind
}

// NewManager creates a Manager with the default timeout.
func NewManager() *Manager {
	return &Manager{
		timeout: DefaultTimeout,
		now:     time.Now,
		after:   time.After,
		stop:    make(chan struct{}),
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
	defer m.logs.Unlock(&m.mu)
	if m.stopped {
		return Dropped
	}

	ss := &m.slots[slotIndex(slot)]
	now := m.now()

	if ss.active == nil || ss.active.id != streamID {
		m.sweepLocked(slot, ss, now)
	}

	kind := KindOther
	if m.kind != nil {
		kind = m.kind(packet)
	}
	voice, data := kind == KindVoice, kind == KindData
	if ss.active == nil && ss.byID[streamID] == nil && (voice || len(ss.pending) == 0) && !m.holdingLocked(ss, kind, now) {
		ss.active = &stream{id: streamID, network: network, sink: sink, voice: voice, data: data, lastSeen: now}
		m.armLocked(slot, ss, now)
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction, m.network).Set(1)
		}
		m.logs.Debug("timeslot acquired",
			"slot", slot, "streamID", streamID, "network", network)
		return Deliver
	}

	if ss.active != nil && ss.active.id == streamID {
		ss.active.lastSeen = now
		ss.active.data = ss.active.data || data
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
		ps = &stream{id: streamID, network: network, sink: sink, voice: voice}
		ss.pending = append(ss.pending, ps)
		if ss.byID == nil {
			ss.byID = make(map[uint]*stream)
		}
		ss.byID[streamID] = ps
		m.logs.Debug("timeslot busy or in call hang, buffering new stream",
			"slot", slot, "pendingStream", streamID, "network", network)
	}
	ps.lastSeen = now
	ps.data = ps.data || data
	v := m.enqueueLocked(slot, ps, packet, end)
	if ss.active == nil {
		m.armLocked(slot, ss, now)
	}
	return v
}

// expirePendingLocked discards waiting calls that went idle.
func (m *Manager) expirePendingLocked(slot bool, ss *slotState, now time.Time) {
	kept := ss.pending[:0]
	for _, p := range ss.pending {
		if !p.expired(now, m.timeout) {
			kept = append(kept, p)
			continue
		}
		m.logs.Info("timeslot discarding idle pending stream",
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
	m.logs.Info("timeslot call timed out, reclaiming slot",
		"slot", slot,
		"oldStream", ss.active.id, "oldNetwork", ss.active.network,
		"pendingCount", len(ss.pending))
	if m.metrics != nil {
		m.metrics.TimeslotTimeouts.WithLabelValues(slotLabel(slot), m.direction, m.network).Inc()
	}
	m.advanceLocked(slot, ss)
}

// armLocked schedules the next stale check on slot: when the active call
// or the first waiting call can expire, or when the call hang that holds
// the waiting calls ends.
func (m *Manager) armLocked(slot bool, ss *slotState, now time.Time) {
	if m.stopped || (ss.active == nil && len(ss.pending) == 0) {
		return
	}
	d := m.timeout
	switch {
	case ss.active == nil:
		d = ss.holdUntil.Sub(now)
	case !ss.draining:
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
	defer m.logs.Unlock(&m.mu)
	if m.stopped {
		return
	}
	ss := &m.slots[slotIndex(slot)]
	now := m.now()
	m.sweepLocked(slot, ss, now)
	if ss.active == nil && len(ss.pending) > 0 && !now.Before(ss.holdUntil) {
		m.advanceLocked(slot, ss)
	}
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
	s.queue = append(s.queue, entry{packet: packet, end: end, at: m.now()})
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
	defer m.logs.Unlock(&m.mu)
	ss := &m.slots[slotIndex(slot)]
	if m.stopped || ss.active == nil || ss.active.id != streamID || ss.draining {
		return
	}
	m.logs.Debug("timeslot released",
		"slot", slot, "streamID", streamID, "network", ss.active.network,
		"pendingCount", len(ss.pending))
	m.advanceLocked(slot, ss)
}

// Stop halts replays and stale-call timers and waits for any delivery in
// progress to return. Packets still buffered are discarded and later
// Submit and Release calls do nothing.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.stopped {
		close(m.stop)
	}
	m.stopped = true
	for i := range m.slots {
		if t := m.slots[i].timer; t != nil {
			t.Stop()
		}
	}
	m.logs.Unlock(&m.mu)
	m.wg.Wait()
}

// advanceLocked ends the active call, promotes the oldest pending call,
// and starts the slot's sender if the promoted call has packets queued.
func (m *Manager) advanceLocked(slot bool, ss *slotState) {
	ss.draining = false
	now := m.now()
	if a := ss.active; a != nil {
		if a.voice && m.hold > 0 {
			ss.holdUntil = later(ss.holdUntil, now.Add(m.hold))
		}
		if a.data && m.dataHold > 0 {
			ss.holdUntil = later(ss.holdUntil, now.Add(m.dataHold))
		}
	}
	i := 0
	if now.Before(ss.holdUntil) {
		i = slices.IndexFunc(ss.pending, func(s *stream) bool { return s.voice })
	}
	if i < 0 || len(ss.pending) == 0 {
		ss.active = nil
		if ss.timer != nil {
			ss.timer.Stop()
		}
		if m.metrics != nil {
			m.metrics.TimeslotActiveCalls.WithLabelValues(slotLabel(slot), m.direction, m.network).Set(0)
		}
		m.armLocked(slot, ss, now)
		return
	}
	next := ss.pending[i]
	ss.pending = slices.Delete(ss.pending, i, i+1)
	delete(ss.byID, next.id)
	next.lastSeen = now
	ss.active = next
	m.logs.Debug("timeslot activating pending stream",
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

// PacketKind is what a packet is for the holds of SetCallHang.
type PacketKind int

const (
	// KindOther is a packet that is neither voice nor data, such as a CSBK.
	KindOther PacketKind = iota
	// KindVoice is a packet of a voice call.
	KindVoice
	// KindData is a data header or data block.
	KindData
)

// SetCallHang makes the manager hold back a call that is not voice, as
// kind tells from its first packet, while hold has not passed since the
// last voice call on its timeslot ended, or dataHold since the last call
// with data on it ended; the later of the two deadlines counts. The held
// call waits as if the slot were busy, and goes out when the hold ends,
// paced as a queued call. A voice call is never held: it starts at once,
// and the held calls then wait for the hold after it. A call without data,
// such as one of preamble CSBKs only, starts no data hold. A hold of 0
// turns it off. An XPR 8400 drops the data and CSBK calls that it gets
// during its call hang after a voice call, and those it gets too soon
// after a data call.
func (m *Manager) SetCallHang(hold, dataHold time.Duration, kind func(packet any) PacketKind) {
	m.mu.Lock()
	defer m.logs.Unlock(&m.mu)
	m.hold, m.dataHold, m.kind = hold, dataHold, kind
}

// holdingLocked reports whether a call on ss whose first packet is of kind
// must wait for a hold at now. The caller must hold m.mu.
func (m *Manager) holdingLocked(ss *slotState, kind PacketKind, now time.Time) bool {
	return kind != KindVoice && now.Before(ss.holdUntil)
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// PaceReplays makes the manager replay a queued call at the spacing its
// packets arrived with, so a replayed call goes out at the pace of a live
// one and leaves room for the calls on the other timeslot. Without it a
// queued call goes out as fast as its sink takes it.
func (m *Manager) PaceReplays() {
	m.mu.Lock()
	defer m.logs.Unlock(&m.mu)
	m.pace = true
}

// Paced reports whether PaceReplays was called.
func (m *Manager) Paced() bool {
	m.mu.Lock()
	defer m.logs.Unlock(&m.mu)
	return m.pace
}

// waitReplay waits, when the manager paces replays, until the queued
// packet of s that arrived at "at" is due: as long after the packet
// replayed before it as it arrived after that packet. It returns false if
// the manager stops first.
func (m *Manager) waitReplay(s *stream, at time.Time) bool {
	if !m.pace {
		return true
	}
	due := m.now()
	if !s.replayedAt.IsZero() {
		due = s.replayedOn.Add(at.Sub(s.replayedAt))
	}
	if wait := due.Sub(m.now()); wait > 0 {
		select {
		case <-m.after(wait):
		case <-m.stop:
			return false
		}
	}
	s.replayedAt, s.replayedOn = at, due
	return true
}

// send delivers the active stream's queued packets in order, paced by
// waitReplay. Packets that arrive
// for the stream while it runs join its queue, so they are delivered
// after the older ones. When a queued packet ends the call, the next
// pending call is promoted and replayed in turn.
func (m *Manager) send(slot bool) {
	defer m.wg.Done()
	ss := &m.slots[slotIndex(slot)]
	for {
		m.mu.Lock()
		s := ss.active
		if m.stopped || s == nil || !ss.draining || len(s.queue) == 0 {
			ss.draining = false
			ss.sending = false
			m.logs.Unlock(&m.mu)
			return
		}
		e := s.queue[0]
		s.queue[0] = entry{}
		s.queue = s.queue[1:]
		s.lastSeen = m.now()
		m.logs.Unlock(&m.mu)

		if !m.waitReplay(s, e.at) {
			m.mu.Lock()
			ss.draining = false
			ss.sending = false
			m.logs.Unlock(&m.mu)
			return
		}
		if s.sink != nil {
			s.sink(e.packet)
		}

		if e.end {
			m.mu.Lock()
			if ss.active == s {
				m.advanceLocked(slot, ss)
			}
			m.logs.Unlock(&m.mu)
		}
	}
}
