package mmdvm

import (
	"testing"
	"time"
)

func TestQuietDataCallEnds(t *testing.T) {
	t.Parallel()
	c, rec := newPreambleTestClient(t)
	call := rigPreambleCall()
	sendRig(t, c, call[12:14], []uint32{105, 105})
	last := time.Now()
	recs := rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 3 })
	if recs[0].burstType != 0x06 || recs[1].burstType != 0x07 || recs[2].burstType != 0x13 || recs[2].raw[17]&0x40 == 0 {
		t.Fatalf("got burst types %02x %02x %02x, want the header, the block and an end packet", recs[0].burstType, recs[1].burstType, recs[2].burstType)
	}
	if recs[2].callControl != recs[0].callControl {
		t.Fatal("end packet is not in the call")
	}
	if d := recs[2].at.Sub(last); d < preambleGap || d > preambleGap+time.Second {
		t.Errorf("end packet %v after the last block", d)
	}
	start := time.Now()
	sendRig(t, c, call[:1], []uint32{5})
	rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 6 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("next call went out after %v", d)
	}
}

func TestEndedDataCallSendsNoEndPacket(t *testing.T) {
	t.Parallel()
	c, rec := newPreambleTestClient(t)
	sendRig(t, c, rigPreambleCall()[12:15], []uint32{105, 105, 105})
	rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 3 })
	time.Sleep(2 * preambleGap)
	recs := rec.snapshot()
	if len(recs) != 3 || recs[2].raw[17]&0x40 == 0 {
		t.Fatalf("got %d packets after a complete data call, want 3 with the end flag on the last", len(recs))
	}
}
