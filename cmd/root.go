package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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
	"github.com/ztrue/shutdown"
)

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

	// Create metrics and optionally start the metrics HTTP server.
	var m *metrics.Metrics
	var metricsSrv *http.Server
	if cfg.Metrics.Enabled && cfg.Metrics.Address != "" {
		m = metrics.NewMetrics()
		mux := http.NewServeMux()
		mux.Handle("/metrics", m.Handler())
		metricsSrv = &http.Server{
			Addr:              cfg.Metrics.Address,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("Starting metrics server", "address", cfg.Metrics.Address)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Metrics server error", "error", err)
			}
		}()
	}

	// Create one MMDVM client per configured network (DMR master).
	// All clients share a single outbound timeslot manager so that
	// only one master can feed a given timeslot toward IPSC at a time.
	outboundTSMgr := timeslot.NewManager()
	if m != nil {
		outboundTSMgr.SetMetrics(m, "outbound")
	}
	mmdvmClients := make([]*mmdvm.MMDVMClient, 0, len(cfg.MMDVM))
	for i := range cfg.MMDVM {
		client := mmdvm.NewMMDVMClient(&cfg.MMDVM[i], m)
		client.SetOutboundTSManager(outboundTSMgr)
		err = client.Start()
		if err != nil {
			return fmt.Errorf("failed to start MMDVM client %q: %w", cfg.MMDVM[i].Name, err)
		}
		mmdvmClients = append(mmdvmClients, client)
	}

	ipscServer := ipsc.NewIPSCServer(cfg, m)

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

	// Wire all MMDVM clients' inbound data to the IPSC server.
	for _, client := range mmdvmClients {
		client.SetIPSCHandler(ipscServer.SendUserPacket)
	}

	err = ipscServer.Start()
	if err != nil {
		return fmt.Errorf("failed to start IPSC server: %w", err)
	}

	stop := func(sig os.Signal) {
		slog.Info("received signal, shutting down...", "signal", sig.String())

		if metricsSrv != nil {
			if err := metricsSrv.Shutdown(context.Background()); err != nil {
				slog.Error("Error shutting down metrics server", "error", err)
			}
		}

		ipscServer.Stop()
		for _, client := range mmdvmClients {
			client.Stop()
		}
	}

	shutdown.AddWithParam(stop)
	shutdown.Listen(syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)

	return nil
}
