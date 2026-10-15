package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/config"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
)

const metricsShutdownTimeout = 5 * time.Second

var errForcedShutdown = errors.New("received a second signal, forcing exit")

func NewCommand(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ipsc2mmdvm",
		Version: fmt.Sprintf("%s - %s", version, commit),
		Annotations: map[string]string{
			"version": version,
			"commit":  commit,
		},
		RunE:              runRoot,
		SilenceErrors:     true,
		SilenceUsage:      true,
		DisableAutoGenTag: true,
	}
	return cmd
}

func runRoot(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	fmt.Printf("ipsc2mmdvm - %s (%s)\n", cmd.Annotations["version"], cmd.Annotations["commit"])

	c, err := configulator.FromContext[config.Config](ctx)
	if err != nil {
		return fmt.Errorf("failed to get config from context")
	}

	cfg, err := c.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	var logger *slog.Logger
	switch cfg.LogLevel {
	case config.LogLevelDebug:
		logger = slog.New(tint.NewTextHandler(os.Stdout, &tint.Options{Level: slog.LevelDebug}))
	case config.LogLevelInfo:
		logger = slog.New(tint.NewTextHandler(os.Stdout, &tint.Options{Level: slog.LevelInfo}))
	case config.LogLevelWarn:
		logger = slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{Level: slog.LevelWarn}))
	case config.LogLevelError:
		logger = slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{Level: slog.LevelError}))
	}
	slog.SetDefault(logger)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	var m *metrics.Metrics
	var metricsSrv *http.Server
	if cfg.Metrics.Enabled {
		m = metrics.NewMetrics()
		mux := http.NewServeMux()
		mux.Handle("/metrics", m.Handler())
		metricsSrv, err = startMetricsServer(ctx, cfg.Metrics.Address, mux)
		if err != nil {
			return err
		}
	}

	ipscServer := ipsc.NewIPSCServer(cfg, m)

	// Create one MMDVM client per configured network (DMR master).
	// All clients share a single outbound timeslot manager so that
	// only one master can feed a given timeslot toward IPSC at a time.
	outboundTSMgr := timeslot.NewManager()
	if m != nil {
		outboundTSMgr.SetMetrics(m, "outbound", timeslot.AllNetworks)
	}
	mmdvmClients := make([]*mmdvm.MMDVMClient, 0, len(cfg.MMDVM))
	for i := range cfg.MMDVM {
		client := mmdvm.NewMMDVMClient(&cfg.MMDVM[i], m)
		client.SetOutboundTSManager(outboundTSMgr)
		client.SetIPSCHandler(ipscServer.SendUserPacket)
		err = client.Start()
		if err != nil {
			return fmt.Errorf("failed to start MMDVM client %q: %w", cfg.MMDVM[i].Name, err)
		}
		mmdvmClients = append(mmdvmClients, client)
	}

	ipscServer.SetBurstHandler(func(packetType byte, data []byte, addr *net.UDPAddr) {
		for _, client := range mmdvmClients {
			if client.MatchesRules(packetType, data, false) {
				dataCopy := make([]byte, len(data))
				copy(dataCopy, data)
				client.HandleIPSCBurst(packetType, dataCopy, addr)
				return
			}
		}
		for _, client := range mmdvmClients {
			if client.MatchesRules(packetType, data, true) {
				dataCopy := make([]byte, len(data))
				copy(dataCopy, data)
				client.HandleIPSCBurst(packetType, dataCopy, addr)
				return
			}
		}
	})

	err = ipscServer.Start()
	if err != nil {
		return fmt.Errorf("failed to start IPSC server: %w", err)
	}

	return waitForShutdown(sigCh, func() {
		if metricsSrv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
			defer cancel()
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
				slog.Error("Error shutting down metrics server", "error", err)
			}
		}

		for _, client := range mmdvmClients {
			client.Stop()
		}
		outboundTSMgr.Stop()
		ipscServer.Stop()
	})
}

// startMetricsServer binds addr before it returns, so a bind failure stops
// startup, then serves handler in the background.
func startMetricsServer(ctx context.Context, addr string, handler http.Handler) (*http.Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen for metrics on %s: %w", addr, err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("Starting metrics server", "address", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Metrics server error", "error", err)
		}
	}()
	return srv, nil
}

// waitForShutdown blocks until a signal arrives on sigCh, then runs stop.
// A second signal while stop runs returns errForcedShutdown at once.
func waitForShutdown(sigCh <-chan os.Signal, stop func()) error {
	sig := <-sigCh
	slog.Info("received signal, shutting down...", "signal", sig.String())

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case sig := <-sigCh:
		return fmt.Errorf("%w: %s", errForcedShutdown, sig)
	}
}
