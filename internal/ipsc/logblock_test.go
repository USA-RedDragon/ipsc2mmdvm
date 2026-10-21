package ipsc

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/logging"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
	"github.com/lmittmann/tint"
)

// stuckWriter blocks every Write until release is closed, as stdout does
// when nothing reads it.
type stuckWriter struct{ release chan struct{} }

func (w stuckWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// TestBlockedLogOutputStallsNothing logs at debug level through tint and
// an AsyncWriter to an output that blocks forever. The translator in both
// directions, the LC rewriter, the timeslot manager and an IPSC send to a
// peer all complete, the records that do not fit are dropped and counted,
// and closing the writer does not hang. It changes the default logger, so
// it does not run in parallel.
func TestBlockedLogOutputStallsNothing(t *testing.T) {
	w := stuckWriter{release: make(chan struct{})}
	defer close(w.release)
	out := logging.NewAsyncWriter(w, 8)
	var drops atomic.Int64
	out.SetOnDrop(func() { drops.Add(1) })
	old := slog.Default()
	slog.SetDefault(slog.New(tint.NewTextHandler(out, &tint.Options{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	s, _ := newTestServerWithUDP(t, false, "")
	registerPeer(t, s, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr := newTestTranslator(t)
		r := NewLCRewriter()
		m := timeslot.NewManager()
		defer m.Stop()
		for i := range 50 {
			tr.TranslateToIPSC(makeTestMMDVMPacket(true, false, dmrd.FrameDataSync, 99))
			tr.TranslateToMMDVM(0x80, make([]byte, 20))
			orig := lcPacket(enums.DataTypeVoiceLCHeader, 1, 2, true, [33]byte{})
			rewritten := orig
			rewritten.Destination = 3
			r.Rewrite(orig, &rewritten)
			m.Submit(false, uint(i), "net1", dmrd.Data{}, true, func(any) {})
			m.Release(false, uint(i))
			s.SendUserPacket(t.Context(), make([]byte, 54))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("packet handling waited for the blocked log output")
	}
	if n := drops.Load(); n == 0 || uint64(n) != out.Dropped() { //nolint:gosec // n is a count
		t.Fatalf("dropped %d, counted %d, want some and the same", out.Dropped(), n)
	}
	closed := make(chan struct{})
	go func() {
		out.Close(100 * time.Millisecond)
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the log writer hung")
	}
}

// firstWriteBlocks blocks the first Write until release is closed, and
// closes blocked when that Write starts.
type firstWriteBlocks struct {
	once    *sync.Once
	blocked chan struct{}
	release chan struct{}
}

func newFirstWriteBlocks() firstWriteBlocks {
	return firstWriteBlocks{once: new(sync.Once), blocked: make(chan struct{}), release: make(chan struct{})}
}

func (w firstWriteBlocks) Write(p []byte) (int, error) {
	first := false
	w.once.Do(func() { first = true })
	if first {
		close(w.blocked)
		<-w.release
	}
	return len(p), nil
}

// TestPacketLocksAreFreeWhileLogging makes the translator and the LC
// rewriter log a debug record into a log output whose first write is
// stuck. Their lock must be free meanwhile, so they log only after they
// release it. It changes the default logger, so it does not run in
// parallel.
func TestPacketLocksAreFreeWhileLogging(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	for _, tc := range []struct {
		name string
		log  func(tr *IPSCTranslator, r *LCRewriter)
		lock func(tr *IPSCTranslator, r *LCRewriter)
	}{
		{"translator to IPSC", func(tr *IPSCTranslator, _ *LCRewriter) {
			tr.TranslateToIPSC(makeTestMMDVMPacket(true, false, dmrd.FrameDataSync, 99))
		}, func(tr *IPSCTranslator, _ *LCRewriter) { tr.SetColorCode(1) }},
		{"translator to MMDVM", func(tr *IPSCTranslator, _ *LCRewriter) {
			tr.TranslateToMMDVM(0x80, make([]byte, 20))
		}, func(tr *IPSCTranslator, _ *LCRewriter) { tr.SetColorCode(1) }},
		{"LC rewriter", func(_ *IPSCTranslator, r *LCRewriter) {
			orig := lcPacket(enums.DataTypeVoiceLCHeader, 1, 2, true, [33]byte{})
			rewritten := orig
			rewritten.Destination = 3
			r.Rewrite(orig, &rewritten)
		}, func(_ *IPSCTranslator, r *LCRewriter) {
			p := lcPacket(enums.DataTypeVoiceLCHeader, 1, 2, true, [33]byte{})
			r.Rewrite(p, &p)
		}},
	} {
		w := newFirstWriteBlocks()
		slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
		tr := newTestTranslator(t)
		r := NewLCRewriter()
		go tc.log(tr, r)
		select {
		case <-w.blocked:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: logged nothing", tc.name)
		}
		free := make(chan struct{})
		go func() {
			tc.lock(tr, r)
			close(free)
		}()
		select {
		case <-free:
		case <-time.After(2 * time.Second):
			t.Errorf("%s: the lock was held while a log write was stuck", tc.name)
		}
		close(w.release)
	}
}
