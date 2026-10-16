package timeslot

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
)

type recorder struct {
	mu  sync.Mutex
	got []any
}

func (r *recorder) sink(p any) {
	r.mu.Lock()
	r.got = append(r.got, p)
	r.mu.Unlock()
}

func (r *recorder) packets() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.got...)
}

// waitPackets waits until the recorder holds want, and fails if it
// holds anything else.
func (r *recorder) waitPackets(t *testing.T, want ...any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := r.packets()
		if reflect.DeepEqual(got, want) {
			return
		}
		if len(got) >= len(want) || time.Now().After(deadline) {
			t.Fatalf("delivered %v, want %v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// expectNone fails if anything is delivered within a short window.
func (r *recorder) expectNone(t *testing.T) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if got := r.packets(); len(got) != 0 {
		t.Fatalf("expected no delivery, got %v", got)
	}
}

func submit(m *Manager, slot bool, id uint, network string, pkt any, r *recorder) bool {
	return m.Submit(slot, id, network, pkt, false, r.sink) == Deliver
}

func submitEnd(m *Manager, id uint, network string, pkt any, r *recorder) bool {
	return m.Submit(false, id, network, pkt, true, r.sink) == Deliver
}

func TestSubmit_FreeSlot(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	if !submit(m, false, 100, "net1", "pkt1", r) {
		t.Fatal("expected to deliver on free TS1")
	}
	if !submit(m, true, 200, "net2", "pkt2", r) {
		t.Fatal("expected to deliver on free TS2")
	}
}

func TestSubmit_SameStream(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	if !submit(m, false, 100, "net1", "pkt1", r) {
		t.Fatal("first submit should deliver")
	}
	if !submit(m, false, 100, "net1", "pkt2", r) {
		t.Fatal("same stream should always deliver")
	}
}

func TestSubmit_DifferentStream_Buffers(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	if !submit(m, false, 100, "net1", "pkt1", r) {
		t.Fatal("first stream should deliver")
	}
	if submit(m, false, 200, "net2", "pkt2", r) {
		t.Fatal("different stream on busy slot should be buffered, not delivered")
	}
}

func TestSubmit_DifferentSlots_Independent(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	if !submit(m, false, 100, "net1", "pkt1", r) {
		t.Fatal("TS1 should be free")
	}
	if !submit(m, true, 200, "net2", "pkt2", r) {
		t.Fatal("TS2 should be independent of TS1")
	}
}

func TestRelease_NoBuffer(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	submit(m, false, 100, "net1", "pkt1", r)
	m.Release(false, 100)
	r.expectNone(t)

	if !submit(m, false, 200, "net2", "pkt2", r) {
		t.Fatal("slot should be free after release")
	}
}

func TestRelease_DeliversBufferedPackets(t *testing.T) {
	m := NewManager()
	a, b := &recorder{}, &recorder{}
	submit(m, false, 100, "net1", "active1", a)
	submit(m, false, 200, "net2", "buffered1", b)
	submit(m, false, 200, "net2", "buffered2", b)

	m.Release(false, 100)
	b.waitPackets(t, "buffered1", "buffered2")
	if got := a.packets(); len(got) != 0 {
		t.Fatalf("buffered packets went to the wrong sink: %v", got)
	}
}

func TestRelease_ActivatesPendingStream(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	submit(m, false, 100, "net1", "a1", r)
	submit(m, false, 200, "net2", "b1", r)
	m.Release(false, 100)
	r.waitPackets(t, "b1")

	if !submit(m, false, 200, "net2", "b2", r) {
		t.Fatal("newly activated pending stream should accept further packets")
	}
	if submit(m, false, 300, "net3", "c1", r) {
		t.Fatal("new stream should be buffered behind activated stream")
	}
}

func TestRelease_FIFO_MultiplePending(t *testing.T) {
	m := NewManager()
	b, c := &recorder{}, &recorder{}
	submit(m, false, 100, "net1", "a1", b)
	submit(m, false, 200, "net2", "b1", b)
	submit(m, false, 200, "net2", "b2", b)
	submit(m, false, 300, "net3", "c1", c)

	m.Release(false, 100)
	b.waitPackets(t, "b1", "b2")
	c.expectNone(t)

	if !submitEnd(m, 200, "net2", "b-end", b) {
		t.Fatal("active stream 200 should deliver its terminator")
	}
	m.Release(false, 200)
	c.waitPackets(t, "c1")
}

func TestRelease_WrongStream(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	submit(m, false, 100, "net1", "a1", r)
	m.Release(false, 999)

	if submit(m, false, 200, "net2", "b1", r) {
		t.Fatal("slot should still be held after release with wrong stream ID")
	}
}

func TestRelease_WrongSlot(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	submit(m, false, 100, "net1", "a1", r)
	m.Release(true, 100)

	if submit(m, false, 200, "net2", "b1", r) {
		t.Fatal("TS1 should still be held after releasing TS2")
	}
}

func TestSubmit_Timeout(t *testing.T) {
	m := NewManager()
	m.timeout = 10 * time.Millisecond
	r := &recorder{}

	submit(m, false, 100, "net1", "a1", r)
	time.Sleep(20 * time.Millisecond)

	if !submit(m, false, 200, "net2", "b1", r) {
		t.Fatal("should reclaim slot after timeout")
	}
}

func TestTimeoutKeepsReclaimingStreamsBufferedPackets(t *testing.T) {
	m := NewManager()
	m.timeout = 20 * time.Millisecond
	a, b := &recorder{}, &recorder{}
	submit(m, false, 1, "A", "A-hdr", a)
	submit(m, false, 2, "B", "B-hdr", b)
	submit(m, false, 2, "B", "B-v1", b)
	time.Sleep(40 * time.Millisecond)

	if submit(m, false, 2, "B", "B-v2", b) {
		b.sink("B-v2")
	}
	b.waitPackets(t, "B-hdr", "B-v1", "B-v2")
}

func TestTimeoutPromotesPendingInOrder(t *testing.T) {
	m := NewManager()
	m.timeout = 100 * time.Millisecond
	t.Cleanup(m.Stop)
	a, b, c := &recorder{}, &recorder{}, &recorder{}
	submit(m, false, 1, "A", "A-hdr", a)
	submit(m, false, 2, "B", "B-hdr", b)
	want := []any{0}
	submit(m, false, 3, "C", 0, c)
	for i := 1; len(b.packets()) == 0; i++ {
		if i > 400 {
			t.Fatal("B was never promoted")
		}
		time.Sleep(5 * time.Millisecond)
		if submit(m, false, 3, "C", i, c) {
			t.Fatal("C must wait behind B, which was queued first")
		}
		want = append(want, i)
	}
	b.waitPackets(t, "B-hdr")
	if submitEnd(m, 2, "B", "B-end", b) {
		m.Release(false, 2)
	}
	c.waitPackets(t, want...)
}

func TestTimerDeliversCompleteCallBehindStaleActive(t *testing.T) {
	m := NewManager()
	m.timeout = 20 * time.Millisecond
	t.Cleanup(m.Stop)
	a, b := &recorder{}, &recorder{}
	submit(m, false, 1, "A", "A-hdr", a)
	submit(m, false, 2, "B", "B-hdr", b)
	submitEnd(m, 2, "B", "B-end", b)

	b.waitPackets(t, "B-hdr", "B-end")
	if !submit(m, false, 3, "C", "C-hdr", &recorder{}) {
		t.Fatal("slot should be free after B's buffered call ended")
	}
}

func TestStopHaltsStaleTimer(t *testing.T) {
	m := NewManager()
	m.timeout = 10 * time.Millisecond
	b := &recorder{}
	submit(m, false, 1, "A", "A-hdr", &recorder{})
	submit(m, false, 2, "B", "B-hdr", b)
	m.Stop()
	time.Sleep(40 * time.Millisecond)
	if got := b.packets(); len(got) != 0 {
		t.Fatalf("timer promoted a call after Stop: %v", got)
	}
}

func TestActiveCallsGaugeResetsAfterLostTerminator(t *testing.T) {
	met := metrics.NewMetrics()
	m := NewManager()
	m.SetMetrics(met, "outbound", AllNetworks)
	m.timeout = 200 * time.Millisecond
	t.Cleanup(m.Stop)
	submit(m, false, 1, "A", "A-hdr", &recorder{})

	const line = `timeslot_active_calls{direction="outbound",network="all",slot="1"} `
	if v := metricLine(t, met, line); v != "1" {
		t.Fatalf("gauge = %q while the call is active, want 1", v)
	}
	deadline := time.Now().Add(2 * time.Second)
	for metricLine(t, met, line) != "0" {
		if time.Now().After(deadline) {
			t.Fatalf("gauge = %q after the call went stale, want 0", metricLine(t, met, line))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func metricLine(t *testing.T, met *metrics.Metrics, prefix string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	met.Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return v
		}
	}
	return ""
}

func newMeteredManager(t *testing.T) (*Manager, *metrics.Metrics) {
	t.Helper()
	met := metrics.NewMetrics()
	m := NewManager()
	m.SetMetrics(met, "outbound", AllNetworks)
	t.Cleanup(m.Stop)
	return m, met
}

func expectDropped(t *testing.T, met *metrics.Metrics, reason, want string) {
	t.Helper()
	prefix := `timeslot_packets_dropped_total{direction="outbound",network="all",reason="` + reason + `",slot="1"} `
	if got := metricLine(t, met, prefix); got != want {
		t.Fatalf("dropped{reason=%q} = %q, want %q", reason, got, want)
	}
}

func TestPendingCallsPerSlotAreCapped(t *testing.T) {
	m, met := newMeteredManager(t)
	submit(m, false, 1, "A", "A-hdr", &recorder{})
	recs := make([]*recorder, MaxPendingCalls+1)
	for i := range recs {
		recs[i] = &recorder{}
		submitEnd(m, uint(10+i), "B", i, recs[i])
	}
	if v := m.Submit(false, 99, "B", "x", false, recs[0].sink); v != Dropped {
		t.Fatalf("call beyond the pending limit got verdict %v, want Dropped", v)
	}
	m.Release(false, 1)
	for i := range MaxPendingCalls {
		recs[i].waitPackets(t, i)
	}
	recs[MaxPendingCalls].expectNone(t)
	expectDropped(t, met, "pending_full", "2")
}

func TestQueuedPacketsPerCallAreCapped(t *testing.T) {
	m, met := newMeteredManager(t)
	submit(m, false, 1, "A", "A-hdr", &recorder{})
	b := &recorder{}
	want := make([]any, 0, MaxQueuedPackets+1)
	for i := range MaxQueuedPackets + 5 {
		submit(m, false, 2, "B", i, b)
		if i < MaxQueuedPackets {
			want = append(want, i)
		}
	}
	submitEnd(m, 2, "B", "B-end", b)
	want = append(want, "B-end")
	m.Release(false, 1)
	b.waitPackets(t, want...)
	expectDropped(t, met, "queue_full", "5")
}

func TestPacketsAfterQueuedTerminatorAreDropped(t *testing.T) {
	m, met := newMeteredManager(t)
	submit(m, false, 1, "A", "A-hdr", &recorder{})
	b := &recorder{}
	submit(m, false, 2, "B", "B-hdr", b)
	submitEnd(m, 2, "B", "B-end", b)
	if v := m.Submit(false, 2, "B", "B-late", false, b.sink); v != Dropped {
		t.Fatalf("packet after a queued terminator got verdict %v, want Dropped", v)
	}
	m.Release(false, 1)
	b.waitPackets(t, "B-hdr", "B-end")
	expectDropped(t, met, "after_end", "1")
}

func TestIdlePendingCallExpires(t *testing.T) {
	m, met := newMeteredManager(t)
	m.timeout = 50 * time.Millisecond
	t.Cleanup(func() { expectDropped(t, met, "expired", "1") })
	a, b := &recorder{}, &recorder{}
	submit(m, false, 1, "A", "A-hdr", a)
	submit(m, false, 2, "B", "B-hdr", b)
	for range 30 {
		time.Sleep(5 * time.Millisecond)
		submit(m, false, 1, "A", "A-voice", a)
	}
	submitEnd(m, 1, "A", "A-end", a)
	m.Release(false, 1)
	b.expectNone(t)
	if !submit(m, false, 3, "C", "C-hdr", &recorder{}) {
		t.Fatal("slot should be free once the idle pending call expired")
	}
}

func TestSubmit_NotYetTimedOut(t *testing.T) {
	m := NewManager()
	m.timeout = 1 * time.Second
	r := &recorder{}

	submit(m, false, 100, "net1", "a1", r)

	if submit(m, false, 200, "net2", "b1", r) {
		t.Fatal("should buffer before timeout")
	}
}

// fakeClock is a clock that moves only when the test advances it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestSubmit_TouchExtendsTimeout(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)
	m.timeout = 30 * time.Millisecond
	clock := &fakeClock{now: time.Unix(1, 0)}
	m.now = clock.Now
	r := &recorder{}

	submit(m, false, 100, "net1", "a1", r)
	clock.Advance(15 * time.Millisecond)

	submit(m, false, 100, "net1", "a2", r)
	clock.Advance(15 * time.Millisecond)

	if submit(m, false, 200, "net2", "b1", r) {
		t.Fatal("slot should still be held; touch extended the timeout")
	}

	clock.Advance(20 * time.Millisecond)
	if submit(m, false, 300, "net3", "c1", r) {
		t.Fatal("stream 300 should wait behind stream 200 after the reclaim")
	}
	r.waitPackets(t, "b1")
}

// nextWait returns the next wait that the manager asked for.
func nextWait(t *testing.T, waits <-chan time.Duration) time.Duration {
	t.Helper()
	select {
	case d := <-waits:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("the replay did not wait")
		return 0
	}
}

// TestReplayKeepsArrivalSpacing queues three packets of stream 200 behind
// stream 100 on TS1, 60 ms apart on a fake clock. When stream 100 ends, the
// sender replays them 60 ms apart instead of at once, and a call on TS2 is
// delivered at once while the replay waits.
func TestReplayKeepsArrivalSpacing(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)
	m.PaceReplays()
	clock := &fakeClock{now: time.Unix(1, 0)}
	m.now = clock.Now
	waits := make(chan time.Duration, 4)
	fire := make(chan time.Time)
	m.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	}
	r := &recorder{}

	submit(m, false, 100, "net1", "a1", r)
	for _, p := range []string{"b1", "b2", "b3"} {
		if submit(m, false, 200, "net2", p, r) {
			t.Fatalf("%s should wait behind stream 100", p)
		}
		clock.Advance(60 * time.Millisecond)
	}
	m.Release(false, 100)

	r.waitPackets(t, "b1")
	if d := nextWait(t, waits); d != 60*time.Millisecond {
		t.Fatalf("replay waited %v before b2, want 60ms", d)
	}
	if !submit(m, true, 300, "net3", "c1", r) {
		t.Fatal("a call on TS2 was not delivered while TS1 replays")
	}
	if got := r.packets(); !reflect.DeepEqual(got, []any{"b1"}) {
		t.Fatalf("delivered %v before b2 was due, want only b1", got)
	}
	clock.Advance(60 * time.Millisecond)
	fire <- clock.Now()
	r.waitPackets(t, "b1", "b2")
	if d := nextWait(t, waits); d != 60*time.Millisecond {
		t.Fatalf("replay waited %v before b3, want 60ms", d)
	}
	clock.Advance(60 * time.Millisecond)
	fire <- clock.Now()
	r.waitPackets(t, "b1", "b2", "b3")
}

// holdManager returns a manager with a 2.5 s call hang hold, in which the
// packets whose names start with "v" are voice, on a fake clock at
// time.Unix(1, 0), with stream 1 a voice call on TS1 that has just ended.
func holdManager(t *testing.T, hold time.Duration) (*Manager, *fakeClock, *recorder) {
	t.Helper()
	m := NewManager()
	t.Cleanup(m.Stop)
	m.PaceReplays()
	m.SetCallHang(hold, func(p any) bool { s, _ := p.(string); return strings.HasPrefix(s, "v") })
	clock := &fakeClock{now: time.Unix(1, 0)}
	m.now = clock.Now
	r := &recorder{}
	if !submit(m, false, 1, "net1", "v1", r) {
		t.Fatal("voice call 1 was not delivered")
	}
	m.Release(false, 1)
	return m, clock, r
}

// TestCallHangHoldsData sends a data call 2.1 s after a voice call on the
// same timeslot. It waits until the 2.5 s hold ends, then goes out.
func TestCallHangHoldsData(t *testing.T) {
	m, clock, r := holdManager(t, 2500*time.Millisecond)
	clock.Advance(2100 * time.Millisecond)
	if submitEnd(m, 2, "net1", "d1", r) {
		t.Fatal("data 2.1 s after voice went out during the hold")
	}
	clock.Advance(390 * time.Millisecond)
	m.onTimer(false)
	r.expectNone(t)
	clock.Advance(20 * time.Millisecond)
	m.onTimer(false)
	r.waitPackets(t, "d1")
}

// TestCallHangLeavesOtherCalls checks the calls that the hold must not
// delay: data 3 s after the voice call, voice during the hold, data on
// the other timeslot during the hold, and data when the hold is 0.
func TestCallHangLeavesOtherCalls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hold  time.Duration
		after time.Duration
		slot  bool
		pkt   string
	}{
		{"data after the hold", 2500 * time.Millisecond, 3 * time.Second, false, "d1"},
		{"voice during the hold", 2500 * time.Millisecond, 2100 * time.Millisecond, false, "v2"},
		{"data on the other slot", 2500 * time.Millisecond, 2100 * time.Millisecond, true, "d1"},
		{"hold off", 0, 2100 * time.Millisecond, false, "d1"},
	} {
		m, clock, r := holdManager(t, tc.hold)
		clock.Advance(tc.after)
		if !submit(m, tc.slot, 2, "net1", tc.pkt, r) {
			t.Errorf("%s: %s was held", tc.name, tc.pkt)
		}
	}
}

// TestCallHangVoiceDuringHoldPushesData holds a data call 1 s after a
// voice call, then starts another voice call. The voice call goes out at
// once, and the data waits for the hold after that call ends.
func TestCallHangVoiceDuringHoldPushesData(t *testing.T) {
	m, clock, r := holdManager(t, 2500*time.Millisecond)
	clock.Advance(time.Second)
	if submitEnd(m, 2, "net1", "d1", r) {
		t.Fatal("data 1 s after voice went out during the hold")
	}
	clock.Advance(500 * time.Millisecond)
	if !submit(m, false, 3, "net1", "v2", r) {
		t.Fatal("voice during the hold was held")
	}
	clock.Advance(1500 * time.Millisecond)
	m.Release(false, 3)
	clock.Advance(2400 * time.Millisecond)
	m.onTimer(false)
	r.expectNone(t)
	clock.Advance(200 * time.Millisecond)
	m.onTimer(false)
	r.waitPackets(t, "d1")
}

func TestRelease_NoOp_EmptySlot(t *testing.T) {
	m := NewManager()
	m.Release(false, 100)
	m.Release(true, 200)
}

func TestSubmit_BothSlotsBusy(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	submit(m, false, 100, "net1", "a1", r)
	submit(m, true, 200, "net2", "b1", r)

	if submit(m, false, 300, "net3", "c1", r) {
		t.Fatal("TS1 should be busy")
	}
	if submit(m, true, 400, "net4", "d1", r) {
		t.Fatal("TS2 should be busy")
	}
}

func TestConcurrentSubmit(t *testing.T) {
	m := NewManager()
	m.timeout = 5 * time.Second
	r := &recorder{}

	const goroutines = 50
	delivered := make(chan uint, goroutines)

	var wg sync.WaitGroup
	for i := range uint(goroutines) {
		wg.Go(func() {
			if submit(m, false, i, "goroutine", "pkt", r) {
				delivered <- i
			}
		})
	}

	wg.Wait()
	close(delivered)

	winners := []uint{}
	for id := range delivered {
		winners = append(winners, id)
	}

	if len(winners) != 1 {
		t.Fatalf("expected exactly 1 winner, got %d: %v", len(winners), winners)
	}
}

func TestRelease_Chain_CompletePendingCall(t *testing.T) {
	m := NewManager()
	a, b, c := &recorder{}, &recorder{}, &recorder{}
	submit(m, false, 100, "net1", "a-header", a)
	submit(m, false, 200, "net2", "b-header", b)
	submit(m, false, 200, "net2", "b-voice", b)
	submitEnd(m, 200, "net2", "b-terminator", b)
	submit(m, false, 300, "net3", "c-header", c)

	m.Release(false, 100)
	b.waitPackets(t, "b-header", "b-voice", "b-terminator")
	c.waitPackets(t, "c-header")

	if !submit(m, false, 300, "net3", "c-voice", c) {
		t.Fatal("stream 300 should be active after the chained replay")
	}
}

func TestSubmit_SameStreamDoesNotDuplicate(t *testing.T) {
	m := NewManager()
	r := &recorder{}
	submit(m, false, 100, "net1", "a1", r)
	submit(m, false, 200, "net2", "b1", r)
	submit(m, false, 200, "net2", "b2", r)
	submit(m, false, 200, "net2", "b3", r)

	m.Release(false, 100)
	r.waitPackets(t, "b1", "b2", "b3")
}

func TestLivePacketsQueueBehindReplay(t *testing.T) {
	m := NewManager()
	blocked := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	r := &recorder{}
	sink := func(p any) {
		once.Do(func() {
			close(blocked)
			<-gate
		})
		r.sink(p)
	}
	m.Submit(false, 100, "net1", "a1", false, sink)
	m.Submit(false, 200, "net2", "b1", false, sink)
	m.Submit(false, 200, "net2", "b2", false, sink)

	released := make(chan struct{})
	go func() {
		m.Release(false, 100)
		close(released)
	}()
	<-blocked
	if v := m.Submit(false, 200, "net2", "b3", false, sink); v != Queued {
		t.Fatalf("live packet during replay got verdict %v, want Queued", v)
	}
	close(gate)
	r.waitPackets(t, "b1", "b2", "b3")
	<-released

	if v := m.Submit(false, 200, "net2", "b4", false, sink); v != Deliver {
		t.Fatalf("live packet after replay got verdict %v, want Deliver", v)
	}
}

func TestReleaseDoesNotWaitForReplay(t *testing.T) {
	m := NewManager()
	gate := make(chan struct{})
	r := &recorder{}
	sink := func(p any) {
		<-gate
		r.sink(p)
	}
	m.Submit(false, 100, "net1", "a1", false, sink)
	m.Submit(false, 200, "net2", "b1", false, sink)

	released := make(chan struct{})
	go func() {
		m.Release(false, 100)
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("Release waited for the replay")
	}
	close(gate)
	r.waitPackets(t, "b1")
	m.Stop()
}

func TestStopWaitsForSenderAndHaltsReplay(t *testing.T) {
	m := NewManager()
	blocked := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	r := &recorder{}
	sink := func(p any) {
		once.Do(func() {
			close(blocked)
			<-gate
		})
		r.sink(p)
	}
	m.Submit(false, 100, "net1", "a1", false, sink)
	m.Submit(false, 200, "net2", "b1", false, sink)
	m.Submit(false, 200, "net2", "b2", false, sink)
	m.Release(false, 100)
	<-blocked

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a delivery was in progress")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	<-stopped
	time.Sleep(20 * time.Millisecond)
	if got := r.packets(); !reflect.DeepEqual(got, []any{"b1"}) {
		t.Fatalf("delivered %v after Stop, want only [b1]", got)
	}
}
