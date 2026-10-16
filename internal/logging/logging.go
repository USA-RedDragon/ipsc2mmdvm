// Package logging keeps logging off the packet paths: an AsyncWriter that
// never blocks the code that logs, and a Queue that holds the records made
// while a lock is held until the lock is released.
package logging

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncWriter writes to w from a goroutine of its own, so a Write never
// waits for w. It holds up to size writes. A write that finds them full is
// dropped and counted, and once w has caught up the writer logs a warning
// with the number dropped.
type AsyncWriter struct {
	w       io.Writer
	queue   chan []byte
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	dropped atomic.Uint64
	onDrop  atomic.Pointer[func()]
}

// NewAsyncWriter returns an AsyncWriter for w that holds up to size
// writes, and starts its goroutine.
func NewAsyncWriter(w io.Writer, size int) *AsyncWriter {
	a := &AsyncWriter{
		w:     w,
		queue: make(chan []byte, size),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go a.run()
	return a
}

// SetOnDrop sets a function that is called for each dropped write, such as
// a metric increment.
func (a *AsyncWriter) SetOnDrop(f func()) {
	a.onDrop.Store(&f)
}

// Dropped returns the number of writes dropped so far.
func (a *AsyncWriter) Dropped() uint64 {
	return a.dropped.Load()
}

// Write queues a copy of p for the writer goroutine and returns at once.
// It drops p when the queue is full. It never returns an error.
func (a *AsyncWriter) Write(p []byte) (int, error) {
	select {
	case a.queue <- append([]byte(nil), p...):
	default:
		a.dropped.Add(1)
		if f := a.onDrop.Load(); f != nil {
			(*f)()
		}
	}
	return len(p), nil
}

func (a *AsyncWriter) run() {
	defer close(a.done)
	var reported uint64
	for {
		select {
		case p := <-a.queue:
			_, _ = a.w.Write(p)
			if n := a.dropped.Load(); n > reported && len(a.queue) == 0 {
				slog.Warn("log records dropped while the log output was blocked", "count", n-reported)
				reported = n
			}
		case <-a.stop:
			for {
				select {
				case p := <-a.queue:
					_, _ = a.w.Write(p)
				default:
					return
				}
			}
		}
	}
}

// Close writes out what is queued and stops the goroutine. It gives up
// after timeout, when w blocks.
func (a *AsyncWriter) Close(timeout time.Duration) {
	a.once.Do(func() { close(a.stop) })
	select {
	case <-a.done:
	case <-time.After(timeout):
	}
}

type record struct {
	level slog.Level
	msg   string
	args  []any
}

// Queue holds log records made while a lock is held, so that they are
// emitted only after the lock is released and a slow log output cannot
// hold the lock. The zero Queue is ready to use. Guard it with the lock it
// serves.
type Queue struct {
	recs []record
}

// Debug queues a debug record, as slog.Debug would log it.
func (q *Queue) Debug(msg string, args ...any) { q.add(slog.LevelDebug, msg, args) }

// Info queues an info record, as slog.Info would log it.
func (q *Queue) Info(msg string, args ...any) { q.add(slog.LevelInfo, msg, args) }

func (q *Queue) add(level slog.Level, msg string, args []any) {
	if !slog.Default().Enabled(context.Background(), level) {
		return
	}
	q.recs = append(q.recs, record{level: level, msg: msg, args: args})
}

// Unlock releases mu, which guards q, and then logs the records queued
// while it was held.
func (q *Queue) Unlock(mu sync.Locker) {
	recs := q.recs
	q.recs = nil
	mu.Unlock()
	for _, r := range recs {
		slog.Log(context.Background(), r.level, r.msg, r.args...)
	}
}
