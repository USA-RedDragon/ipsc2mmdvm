package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	labelDirection = "direction"
	labelNetwork   = "network"
	labelSlot      = "slot"
)

// Metrics holds all Prometheus collectors for the application.
// Pass a non-nil *Metrics to components that should be instrumented;
// nil is safe and acts as a no-op (callers must nil-check).
type Metrics struct {
	registry *prometheus.Registry

	// IPSC Server
	IPSCPacketsReceived *prometheus.CounterVec
	IPSCPacketsSent     prometheus.Counter
	IPSCPeersRegistered prometheus.Gauge
	IPSCAuthFailures    prometheus.Counter
	IPSCUDPErrors       *prometheus.CounterVec

	// MMDVM Client
	MMDVMConnectionState *prometheus.GaugeVec
	MMDVMReconnects      *prometheus.CounterVec
	MMDVMAuthFailures    *prometheus.CounterVec
	MMDVMPingRTT         *prometheus.HistogramVec
	MMDVMPacketsReceived *prometheus.CounterVec
	MMDVMPacketsSent     *prometheus.CounterVec
	MMDVMPacketsDropped  *prometheus.CounterVec

	// Rewrite
	MMDVMRewriteMatches *prometheus.CounterVec

	// Timeslot Manager
	TimeslotActiveCalls     *prometheus.GaugeVec
	TimeslotPacketsBuffered *prometheus.CounterVec
	TimeslotTimeouts        *prometheus.CounterVec

	// Translator
	TranslatorActiveStreams *prometheus.GaugeVec
	TranslatorPackets       *prometheus.CounterVec
}

// NewMetrics creates and registers all application metrics with a
// dedicated registry (no global state). Includes Go runtime and
// process collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		registry: reg,

		// IPSC Server
		IPSCPacketsReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ipsc_packets_received_total",
			Help: "Total IPSC packets received by type.",
		}, []string{"type"}),
		IPSCPacketsSent: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ipsc_packets_sent_total",
			Help: "Total IPSC packets sent to peers.",
		}),
		IPSCPeersRegistered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ipsc_peers_registered",
			Help: "Number of currently registered IPSC peers.",
		}),
		IPSCAuthFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ipsc_auth_failures_total",
			Help: "Total IPSC authentication failures.",
		}),
		IPSCUDPErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ipsc_udp_errors_total",
			Help: "Total IPSC UDP errors by direction.",
		}, []string{labelDirection}),

		// MMDVM Client
		MMDVMConnectionState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mmdvm_connection_state",
			Help: "MMDVM connection state (0=disconnected, 1=connecting, 2=ready).",
		}, []string{labelNetwork}),
		MMDVMReconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mmdvm_reconnects_total",
			Help: "Total MMDVM reconnections.",
		}, []string{labelNetwork}),
		MMDVMAuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mmdvm_auth_failures_total",
			Help: "Total MMDVM authentication failures.",
		}, []string{labelNetwork}),
		MMDVMPingRTT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mmdvm_ping_rtt_seconds",
			Help:    "MMDVM ping round-trip time in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.0},
		}, []string{labelNetwork}),
		MMDVMPacketsReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mmdvm_packets_received_total",
			Help: "Total MMDVM DMRD packets received.",
		}, []string{labelNetwork}),
		MMDVMPacketsSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mmdvm_packets_sent_total",
			Help: "Total MMDVM DMRD packets sent.",
		}, []string{labelNetwork}),
		MMDVMPacketsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mmdvm_packets_dropped_total",
			Help: "Total MMDVM packets dropped by reason.",
		}, []string{labelNetwork, "reason"}),

		// Rewrite
		MMDVMRewriteMatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mmdvm_rewrite_matches_total",
			Help: "Total rewrite rule matches.",
		}, []string{labelNetwork, labelDirection, "type"}),

		// Timeslot Manager
		TimeslotActiveCalls: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "timeslot_active_calls",
			Help: "Number of active calls per timeslot.",
		}, []string{labelSlot, labelDirection}),
		TimeslotPacketsBuffered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "timeslot_packets_buffered_total",
			Help: "Total packets buffered due to timeslot contention.",
		}, []string{labelSlot, labelDirection}),
		TimeslotTimeouts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "timeslot_timeouts_total",
			Help: "Total timeslot call timeouts.",
		}, []string{labelSlot, labelDirection}),

		// Translator
		TranslatorActiveStreams: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "translator_active_streams",
			Help: "Number of active translator streams by direction.",
		}, []string{labelDirection}),
		TranslatorPackets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "translator_packets_total",
			Help: "Total packets translated by direction.",
		}, []string{labelDirection}),
	}

	reg.MustRegister(
		m.IPSCPacketsReceived,
		m.IPSCPacketsSent,
		m.IPSCPeersRegistered,
		m.IPSCAuthFailures,
		m.IPSCUDPErrors,
		m.MMDVMConnectionState,
		m.MMDVMReconnects,
		m.MMDVMAuthFailures,
		m.MMDVMPingRTT,
		m.MMDVMPacketsReceived,
		m.MMDVMPacketsSent,
		m.MMDVMPacketsDropped,
		m.MMDVMRewriteMatches,
		m.TimeslotActiveCalls,
		m.TimeslotPacketsBuffered,
		m.TimeslotTimeouts,
		m.TranslatorActiveStreams,
		m.TranslatorPackets,
	)

	return m
}

// Handler returns an http.Handler that serves the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
