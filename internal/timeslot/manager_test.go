package timeslot

import (
	"reflect"
	"sync"
	"testing"
	"time"
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

func submitEnd(m *Manager, slot bool, id uint, network string, pkt any, r *recorder) bool {
	return m.Submit(slot, id, network, pkt, true, r.sink) == Deliver
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

	if !submitEnd(m, false, 200, "net2", "b-end", b) {
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
	m.timeout = 20 * time.Millisecond
	a, b, c := &recorder{}, &recorder{}, &recorder{}
	submit(m, false, 1, "A", "A-hdr", a)
	submit(m, false, 2, "B", "B-hdr", b)
	submit(m, false, 3, "C", "C-hdr", c)
	time.Sleep(40 * time.Millisecond)

	if submit(m, false, 3, "C", "C-v1", c) {
		t.Fatal("C must wait behind B, which was queued first")
	}
	b.waitPackets(t, "B-hdr")
	if !submitEnd(m, false, 2, "B", "B-end", b) {
		t.Fatal("B should be active after the reclaim")
	}
	m.Release(false, 2)
	c.waitPackets(t, "C-hdr", "C-v1")
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

func TestSubmit_TouchExtendsTimeout(t *testing.T) {
	m := NewManager()
	m.timeout = 30 * time.Millisecond
	r := &recorder{}

	submit(m, false, 100, "net1", "a1", r)
	time.Sleep(15 * time.Millisecond)

	submit(m, false, 100, "net1", "a2", r)
	time.Sleep(15 * time.Millisecond)

	if submit(m, false, 200, "net2", "b1", r) {
		t.Fatal("slot should still be held; touch extended the timeout")
	}

	time.Sleep(20 * time.Millisecond)
	if submit(m, false, 300, "net3", "c1", r) {
		t.Fatal("stream 300 should wait behind stream 200 after the reclaim")
	}
	r.waitPackets(t, "b1")
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
	submitEnd(m, false, 200, "net2", "b-terminator", b)
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
