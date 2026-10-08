package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewMetrics(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	if m == nil {
		t.Fatal("expected non-nil Metrics")
	}
	if m.registry == nil {
		t.Fatal("expected non-nil registry")
	}
}

func TestHandler_ContainsGoMetrics(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	handler := m.Handler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	if !strings.Contains(string(body), "go_goroutines") {
		t.Fatal("expected go_goroutines metric in output")
	}
}

func TestHandler_ContainsCustomMetrics(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	// Increment a few counters to see them in output
	m.IPSCPacketsReceived.WithLabelValues("group_voice").Inc()
	m.MMDVMConnectionState.WithLabelValues("TestNet").Set(2)
	m.MMDVMReconnects.WithLabelValues("TestNet").Inc()
	m.TimeslotActiveCalls.WithLabelValues("1", "outbound").Set(1)
	m.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Set(1)
	handler := m.Handler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"ipsc_packets_received_total",
		"mmdvm_connection_state",
		"ipsc_packets_sent_total",
		"mmdvm_reconnects_total",
		"timeslot_active_calls",
		"translator_active_streams",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in metrics output", want)
		}
	}
}
