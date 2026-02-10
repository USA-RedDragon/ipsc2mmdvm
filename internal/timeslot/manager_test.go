package timeslot

import (
	"testing"
	"time"
)

func TestSubmit_FreeSlot(t *testing.T) {
	m := NewManager()
	if !m.Submit(false, 100, "net1", "pkt1") {
		t.Fatal("expected to deliver on free TS1")
	}
	if !m.Submit(true, 200, "net2", "pkt2") {
		t.Fatal("expected to deliver on free TS2")
	}
}

func TestSubmit_SameStream(t *testing.T) {
	m := NewManager()
	if !m.Submit(false, 100, "net1", "pkt1") {
		t.Fatal("first submit should deliver")
	}
	if !m.Submit(false, 100, "net1", "pkt2") {
		t.Fatal("same stream should always deliver")
	}
}

func TestSubmit_DifferentStream_Buffers(t *testing.T) {
	m := NewManager()
	if !m.Submit(false, 100, "net1", "pkt1") {
		t.Fatal("first stream should deliver")
	}
	if m.Submit(false, 200, "net2", "pkt2") {
		t.Fatal("different stream on busy slot should be buffered, not delivered")
	}
}

func TestSubmit_DifferentSlots_Independent(t *testing.T) {
	m := NewManager()
	if !m.Submit(false, 100, "net1", "pkt1") {
		t.Fatal("TS1 should be free")
	}
	if !m.Submit(true, 200, "net2", "pkt2") {
		t.Fatal("TS2 should be independent of TS1")
	}
}

func TestRelease_NoBuffer(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "pkt1")
	buffered := m.Release(false, 100)

	if len(buffered) != 0 {
		t.Fatalf("expected no buffered packets, got %d", len(buffered))
	}

	if !m.Submit(false, 200, "net2", "pkt2") {
		t.Fatal("slot should be free after release")
	}
}

func TestRelease_ReturnsBufferedPackets(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "active1")
	m.Submit(false, 200, "net2", "buffered1")
	m.Submit(false, 200, "net2", "buffered2")

	buffered := m.Release(false, 100)
	if len(buffered) != 2 {
		t.Fatalf("expected 2 buffered packets, got %d", len(buffered))
	}
	if buffered[0].(string) != "buffered1" || buffered[1].(string) != "buffered2" {
		t.Fatalf("unexpected packet contents: %v", buffered)
	}
}

func TestRelease_ActivatesPendingStream(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "a1")
	m.Submit(false, 200, "net2", "b1")
	m.Release(false, 100) // activates stream 200

	// Stream 200 is now active — new packets should deliver.
	if !m.Submit(false, 200, "net2", "b2") {
		t.Fatal("newly activated pending stream should accept further packets")
	}

	// Different stream should be buffered behind 200.
	if m.Submit(false, 300, "net3", "c1") {
		t.Fatal("new stream should be buffered behind activated stream")
	}
}

func TestRelease_FIFO_MultiplePending(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "a1")
	m.Submit(false, 200, "net2", "b1")
	m.Submit(false, 200, "net2", "b2")
	m.Submit(false, 300, "net3", "c1")

	// Release active (100) → get stream 200's packets.
	buffered := m.Release(false, 100)
	if len(buffered) != 2 {
		t.Fatalf("expected 2 packets from stream 200, got %d", len(buffered))
	}

	// Release stream 200 → get stream 300's packets.
	buffered = m.Release(false, 200)
	if len(buffered) != 1 {
		t.Fatalf("expected 1 packet from stream 300, got %d", len(buffered))
	}
	if buffered[0].(string) != "c1" {
		t.Fatalf("expected 'c1', got %v", buffered[0])
	}
}

func TestRelease_WrongStream(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "a1")
	buffered := m.Release(false, 999)

	if buffered != nil {
		t.Fatal("release with wrong stream ID should return nil")
	}

	// Slot should still be held.
	if m.Submit(false, 200, "net2", "b1") {
		t.Fatal("slot should still be held after release with wrong stream ID")
	}
}

func TestRelease_WrongSlot(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "a1")
	m.Release(true, 100) // wrong slot

	if m.Submit(false, 200, "net2", "b1") {
		t.Fatal("TS1 should still be held after releasing TS2")
	}
}

func TestSubmit_Timeout(t *testing.T) {
	m := NewManager()
	m.timeout = 10 * time.Millisecond

	m.Submit(false, 100, "net1", "a1")
	time.Sleep(20 * time.Millisecond)

	if !m.Submit(false, 200, "net2", "b1") {
		t.Fatal("should reclaim slot after timeout")
	}
}

func TestSubmit_Timeout_DiscardsPending(t *testing.T) {
	m := NewManager()
	m.timeout = 10 * time.Millisecond

	m.Submit(false, 100, "net1", "a1")
	m.Submit(false, 200, "net2", "b1") // buffered
	time.Sleep(20 * time.Millisecond)

	// Timeout reclaims and discards pending.
	if !m.Submit(false, 300, "net3", "c1") {
		t.Fatal("should reclaim slot after timeout")
	}

	// Release 300 should return nothing (200 was discarded).
	buffered := m.Release(false, 300)
	if len(buffered) != 0 {
		t.Fatalf("expected no buffered packets after timeout reclaim, got %d", len(buffered))
	}
}

func TestSubmit_NotYetTimedOut(t *testing.T) {
	m := NewManager()
	m.timeout = 1 * time.Second

	m.Submit(false, 100, "net1", "a1")

	if m.Submit(false, 200, "net2", "b1") {
		t.Fatal("should buffer before timeout")
	}
}

func TestSubmit_TouchExtendsTimeout(t *testing.T) {
	m := NewManager()
	m.timeout = 30 * time.Millisecond

	m.Submit(false, 100, "net1", "a1")
	time.Sleep(15 * time.Millisecond)

	m.Submit(false, 100, "net1", "a2") // touch extends timeout
	time.Sleep(15 * time.Millisecond)

	// Only 15ms since last touch — should still be buffered.
	if m.Submit(false, 200, "net2", "b1") {
		t.Fatal("slot should still be held; touch extended the timeout")
	}

	time.Sleep(20 * time.Millisecond)
	if !m.Submit(false, 200, "net2", "b2") {
		t.Fatal("should reclaim slot after extended timeout expires")
	}
}

func TestRelease_NoOp_EmptySlot(t *testing.T) {
	m := NewManager()
	// Releasing a slot that was never acquired should not panic.
	buffered := m.Release(false, 100)
	if buffered != nil {
		t.Fatal("release on empty slot should return nil")
	}
	buffered = m.Release(true, 200)
	if buffered != nil {
		t.Fatal("release on empty slot should return nil")
	}
}

func TestSubmit_BothSlotsBusy(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "a1")
	m.Submit(true, 200, "net2", "b1")

	if m.Submit(false, 300, "net3", "c1") {
		t.Fatal("TS1 should be busy")
	}
	if m.Submit(true, 400, "net4", "d1") {
		t.Fatal("TS2 should be busy")
	}
}

func TestConcurrentSubmit(t *testing.T) {
	m := NewManager()
	m.timeout = 5 * time.Second

	const goroutines = 50
	delivered := make(chan uint, goroutines)

	for i := range goroutines {
		go func(id uint) {
			if m.Submit(false, id, "goroutine", "pkt") {
				delivered <- id
			}
		}(uint(i))
	}

	time.Sleep(50 * time.Millisecond)
	close(delivered)

	// Exactly one goroutine should have delivered immediately.
	winners := []uint{}
	for id := range delivered {
		winners = append(winners, id)
	}

	if len(winners) != 1 {
		t.Fatalf("expected exactly 1 winner, got %d: %v", len(winners), winners)
	}
}

func TestRelease_Chain_CompletePendingCall(t *testing.T) {
	// Simulate: stream 100 active, stream 200 buffered with a complete
	// call (header + terminator). After releasing 100, the caller gets
	// 200's packets, sees the terminator, and releases 200 to chain.
	m := NewManager()
	m.Submit(false, 100, "net1", "a-header")
	m.Submit(false, 200, "net2", "b-header")
	m.Submit(false, 200, "net2", "b-voice")
	m.Submit(false, 200, "net2", "b-terminator")
	m.Submit(false, 300, "net3", "c-header")

	// Release 100 → get 200's packets.
	buffered := m.Release(false, 100)
	if len(buffered) != 3 {
		t.Fatalf("expected 3 packets from stream 200, got %d", len(buffered))
	}
	if buffered[0].(string) != "b-header" {
		t.Fatalf("expected b-header, got %v", buffered[0])
	}
	if buffered[2].(string) != "b-terminator" {
		t.Fatalf("expected b-terminator, got %v", buffered[2])
	}

	// Caller detects terminator in buffered → release 200 → get 300's packets.
	buffered = m.Release(false, 200)
	if len(buffered) != 1 {
		t.Fatalf("expected 1 packet from stream 300, got %d", len(buffered))
	}
	if buffered[0].(string) != "c-header" {
		t.Fatalf("expected c-header, got %v", buffered[0])
	}
}

func TestSubmit_SameStreamDoesNotDuplicate(t *testing.T) {
	m := NewManager()
	m.Submit(false, 100, "net1", "a1")
	m.Submit(false, 200, "net2", "b1")
	m.Submit(false, 200, "net2", "b2") // same pending stream
	m.Submit(false, 200, "net2", "b3") // same pending stream

	buffered := m.Release(false, 100)
	if len(buffered) != 3 {
		t.Fatalf("expected 3 buffered packets from stream 200, got %d", len(buffered))
	}
}
