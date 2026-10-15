package mmdvm

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedMaster answers every datagram with reply(data). It counts RPTLs.
func scriptedMaster(t *testing.T, reply func(data []byte) []byte) (string, *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	var logins atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			data := buf[:n]
			if bytes.HasPrefix(data, []byte(tagRPTL)) {
				logins.Add(1)
			}
			if r := reply(data); r != nil {
				_, _ = conn.WriteToUDP(r, addr)
			}
		}
	}()
	return conn.LocalAddr().String(), &logins
}

func isRPTC(d []byte) bool {
	return bytes.HasPrefix(d, []byte(tagRPTC)) && !bytes.HasPrefix(d, []byte(tagRPTCL))
}

func acceptAll(d []byte) []byte {
	switch {
	case bytes.HasPrefix(d, []byte(tagRPTL)):
		return []byte("RPTACK\x01\x02\x03\x04")
	case bytes.HasPrefix(d, []byte(tagRPTK)), isRPTC(d):
		return []byte("RPTACK\x00\x04\xC2\x34")
	}
	return nil
}

const testMSTCL = "MSTCL\x00\x04\xC2\x34"

func TestMasterRejectionDoesNotLoopLogins(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		reply func([]byte) []byte
	}{
		{"MSTCL to RPTL", func(d []byte) []byte {
			if bytes.HasPrefix(d, []byte(tagRPTL)) {
				return []byte(testMSTCL)
			}
			return nil
		}},
		{"MSTCL to RPTPING", func(d []byte) []byte {
			if bytes.HasPrefix(d, []byte(tagRPTPING)) {
				return []byte(testMSTCL)
			}
			return acceptAll(d)
		}},
		{"MSTNAK to RPTPING", func(d []byte) []byte {
			if bytes.HasPrefix(d, []byte(tagRPTPING)) {
				return []byte(testMSTNAK)
			}
			return acceptAll(d)
		}},
		{"MSTNAK to RPTC", func(d []byte) []byte {
			if isRPTC(d) {
				return []byte(testMSTNAK)
			}
			return acceptAll(d)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, logins := scriptedMaster(t, tc.reply)
			cfg := testMMDVMConfig()
			cfg.MasterServer = addr
			c := NewMMDVMClient(cfg, nil)
			if err := c.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			time.Sleep(time.Second)
			c.Stop()
			if n := logins.Load(); n != 1 {
				t.Fatalf("expected 1 RPTL in 1s at a 5s keepalive, got %d", n)
			}
		})
	}
}

func TestReconnectAfterMSTCLWaitsForRetryInterval(t *testing.T) {
	t.Parallel()
	addr, logins := scriptedMaster(t, func(d []byte) []byte {
		if bytes.HasPrefix(d, []byte(tagRPTL)) {
			return []byte(testMSTCL)
		}
		return nil
	})
	cfg := testMMDVMConfig()
	cfg.MasterServer = addr
	c := NewMMDVMClient(cfg, nil)
	c.keepAlive = 50 * time.Millisecond
	c.timeout = time.Hour
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(time.Second)
	c.Stop()
	limit := 1 + int(time.Second/c.keepAlive)
	if n := int(logins.Load()); n < 2 || n > limit {
		t.Fatalf("expected 2 to %d RPTLs in 1s at a 50ms keepalive, got %d", limit, n)
	}
}
