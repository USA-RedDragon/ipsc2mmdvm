package mmdvm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd/session"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
)

func metricsText(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func startMetricsClient(t *testing.T, fm *fakeMaster) (*MMDVMClient, *metrics.Metrics) {
	t.Helper()
	cfg := testMMDVMConfig()
	cfg.MasterServer = fm.addr()
	m := metrics.NewMetrics()
	c := NewMMDVMClient(cfg, m)
	c.retry = 25 * time.Millisecond
	c.timeout = 300 * time.Millisecond
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Stop)
	return c, m
}

func TestSessionLogsInAndPings(t *testing.T) {
	t.Parallel()
	fm := newFakeMaster(t)
	c, m := startMetricsClient(t, fm)
	waitFor(t, 2*time.Second, "READY", isReady(c))
	waitFor(t, 2*time.Second, "pings", func() bool { return fm.pings.Load() >= 3 })
	waitFor(t, time.Second, "ping RTT", func() bool {
		return !strings.Contains(metricsText(t, m), `mmdvm_ping_rtt_seconds_count{network="TestNet"} 0`) &&
			strings.Contains(metricsText(t, m), `mmdvm_ping_rtt_seconds_count{network="TestNet"}`)
	})
	if body := metricsText(t, m); !strings.Contains(body, `mmdvm_connection_state{network="TestNet"} 2`) {
		t.Fatal("expected connection state 2 when logged in")
	}
}

func TestStopSendsRPTCL(t *testing.T) {
	t.Parallel()
	fm := newFakeMaster(t)
	c := startFastClient(t, fm)
	waitFor(t, 2*time.Second, "READY", isReady(c))
	c.Stop()
	waitFor(t, time.Second, "RPTCL", func() bool { return fm.rptcls.Load() == 1 })
}

func TestStopBeforeLoginSendsNoRPTCL(t *testing.T) {
	t.Parallel()
	fm := newFakeMaster(t)
	fm.answer.Store(false)
	c := startFastClient(t, fm)
	waitFor(t, 2*time.Second, "RPTL", func() bool { return fm.logins.Load() >= 1 })
	c.Stop()
	time.Sleep(50 * time.Millisecond)
	if n := fm.rptcls.Load(); n != 0 {
		t.Fatalf("expected no RPTCL before login, got %d", n)
	}
}

func TestAuthRejectionCountsFailure(t *testing.T) {
	t.Parallel()
	fm := newFakeMaster(t)
	fm.nakAuth.Store(true)
	c, m := startMetricsClient(t, fm)
	waitFor(t, 2*time.Second, "auth failure", func() bool {
		return strings.Contains(metricsText(t, m), `mmdvm_auth_failures_total{network="TestNet"}`)
	})
	if c.up.Load() {
		t.Fatal("logged in with a rejected password")
	}
	fm.nakAuth.Store(false)
	waitFor(t, 2*time.Second, "READY after the password is accepted", isReady(c))
}

func TestMSTCLDialsNewConnection(t *testing.T) {
	t.Parallel()
	fm := newFakeMaster(t)
	c, m := startMetricsClient(t, fm)
	waitFor(t, 2*time.Second, "READY", isReady(c))
	before := fm.peer.Load().String()
	fm.send(t, []byte(testMSTCL))
	waitFor(t, time.Second, "session end", func() bool { return !c.up.Load() })
	waitFor(t, 2*time.Second, "READY again", isReady(c))
	if after := fm.peer.Load().String(); after == before {
		t.Fatalf("expected a new connection after MSTCL, still %s", after)
	}
	if !strings.Contains(metricsText(t, m), `mmdvm_reconnects_total{network="TestNet"} 1`) {
		t.Fatal("expected one reconnect")
	}
}

func TestMSTNAKLogsInOnSameConnection(t *testing.T) {
	t.Parallel()
	fm := newFakeMaster(t)
	c := startFastClient(t, fm)
	waitFor(t, 2*time.Second, "READY", isReady(c))
	before := fm.peer.Load().String()
	logins := fm.logins.Load()
	fm.send(t, []byte(testMSTNAK))
	waitFor(t, time.Second, "session end", func() bool { return !c.up.Load() })
	waitFor(t, 2*time.Second, "READY again", isReady(c))
	if fm.logins.Load() <= logins {
		t.Fatal("expected a new RPTL after MSTNAK")
	}
	if after := fm.peer.Load().String(); after != before {
		t.Fatalf("expected the same connection after MSTNAK, %s became %s", before, after)
	}
}

func TestPacketFromClosedConnectionIsDropped(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	if err := c.newPeer(); err != nil {
		t.Fatal(err)
	}
	c.peerMu.Lock()
	c.peer.Tick(0, nil)
	c.peerMu.Unlock()
	c.fromMaster(rxPacket{gen: c.connGen.Load() + 1, data: []byte("RPTACK\x01\x02\x03\x04")}, nil)
	if st := c.peer.State(); st != session.StateLogin {
		t.Fatalf("a packet from a closed connection moved the session to %s", st)
	}
	c.fromMaster(rxPacket{gen: c.connGen.Load(), data: []byte("RPTACK\x01\x02\x03\x04")}, nil)
	if st := c.peer.State(); st != session.StateAuth {
		t.Fatalf("expected the RPTACK of the current connection to move the session to Auth, got %s", st)
	}
}

func TestReconnectDiscardsQueuedPackets(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	for len(c.connRX) < cap(c.connRX) {
		c.connRX <- rxPacket{data: []byte(testMSTCL)}
	}
	gen := c.connGen.Load()
	c.peerMu.Lock()
	c.reconnect()
	c.peerMu.Unlock()
	if n := len(c.connRX); n != 0 {
		t.Fatalf("expected no queued packets after reconnect, got %d", n)
	}
	if c.connGen.Load() != gen {
		t.Fatal("reconnect without a connection changed the generation")
	}
}
