package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateWriter blocks every Write until open is closed.
type gateWriter struct {
	open chan struct{}
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (g *gateWriter) Write(p []byte) (int, error) {
	<-g.open
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *gateWriter) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

// TestAsyncWriterNeverBlocks writes to an output that blocks. Every Write
// returns at once, the ones that find the queue full are dropped and
// counted, and Close gives up at its timeout.
func TestAsyncWriterNeverBlocks(t *testing.T) {
	t.Parallel()
	g := &gateWriter{open: make(chan struct{})}
	defer close(g.open)
	a := NewAsyncWriter(g, 4)
	var drops uint64
	a.SetOnDrop(func() { drops++ })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			_, _ = a.Write([]byte("x\n"))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a blocked output")
	}
	if a.Dropped() < 90 || drops != a.Dropped() {
		t.Fatalf("dropped %d, counted %d, want at least 90 and the same", a.Dropped(), drops)
	}
	start := time.Now()
	a.Close(50 * time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v on a blocked output", d)
	}
}

// TestAsyncWriterFlushesAndReportsDrops opens a blocked output after some
// writes were dropped. The queued writes go out, then a warning with the
// number dropped, and Close writes out the rest.
func TestAsyncWriterFlushesAndReportsDrops(t *testing.T) {
	g := &gateWriter{open: make(chan struct{})}
	a := NewAsyncWriter(g, 2)
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(a, nil)))
	defer slog.SetDefault(old)
	for i := range 6 {
		_, _ = a.Write([]byte{'a' + byte(i), '\n'})
	}
	close(g.open)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(g.String(), "count=") {
		if time.Now().After(deadline) {
			t.Fatalf("no drop warning in %q", g.String())
		}
		time.Sleep(time.Millisecond)
	}
	_, _ = a.Write([]byte("last\n"))
	a.Close(time.Second)
	out := g.String()
	if !strings.Contains(out, "count=") || !strings.HasSuffix(out, "last\n") {
		t.Fatalf("output %q, want the drop warning and the last write", out)
	}
}

// TestQueueLogsAfterUnlock queues records under a lock and checks that
// they are logged only after Unlock releases it.
func TestQueueLogsAfterUnlock(t *testing.T) {
	var mu sync.Mutex
	var q Queue
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockCheck{mu: &mu, t: t, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	mu.Lock()
	q.Debug("one", "k", 1)
	q.Info("two")
	if buf.Len() != 0 {
		t.Fatal("a record was logged while the lock was held")
	}
	q.Unlock(&mu)
	if out := buf.String(); !strings.Contains(out, "msg=one k=1") || !strings.Contains(out, "msg=two") {
		t.Fatalf("logged %q", out)
	}
}

// lockCheck fails the test when it is written to while mu is held.
type lockCheck struct {
	mu *sync.Mutex
	t  *testing.T
	w  *bytes.Buffer
}

func (l *lockCheck) Write(p []byte) (int, error) {
	if l.mu.TryLock() {
		l.mu.Unlock()
	} else {
		l.t.Error("logged while the lock was held")
	}
	return l.w.Write(p)
}
