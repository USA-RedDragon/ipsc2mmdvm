package cmd

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"syscall"

	"github.com/USA-RedDragon/configulator"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/config"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/hbrp"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/ipsc"
	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
	"github.com/ztrue/shutdown"
)

func NewCommand(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ipsc2hbrp",
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
	fmt.Printf("ipsc2hbrp - %s (%s)\n", cmd.Annotations["version"], cmd.Annotations["commit"])

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
		logger = slog.New(tint.NewHandler(os.Stdout, &tint.Options{Level: slog.LevelDebug}))
	case config.LogLevelInfo:
		logger = slog.New(tint.NewHandler(os.Stdout, &tint.Options{Level: slog.LevelInfo}))
	case config.LogLevelWarn:
		logger = slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelWarn}))
	case config.LogLevelError:
		logger = slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelError}))
	}
	slog.SetDefault(logger)

	// Create one HBRP client per configured network (DMR master).
	hbrpClients := make([]*hbrp.HBRPClient, 0, len(cfg.HBRP))
	for i := range cfg.HBRP {
		client := hbrp.NewHBRPClient(&cfg.HBRP[i])
		err = client.Start()
		if err != nil {
			return fmt.Errorf("failed to start HBRP client %q: %w", cfg.HBRP[i].Name, err)
		}
		hbrpClients = append(hbrpClients, client)
	}

	ipscServer := ipsc.NewIPSCServer(cfg)

	// Wire IPSC bursts to all HBRP clients.
	ipscServer.SetBurstHandler(func(packetType byte, data []byte, addr *net.UDPAddr) {
		for _, client := range hbrpClients {
			// Each client has its own copy of data and applies its own rewrite rules.
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)
			client.HandleIPSCBurst(packetType, dataCopy, addr)
		}
	})

	// Wire all HBRP clients' inbound data to the IPSC server.
	for _, client := range hbrpClients {
		client.SetIPSCHandler(ipscServer.SendUserPacket)
	}

	err = ipscServer.Start()
	if err != nil {
		return fmt.Errorf("failed to start IPSC server: %w", err)
	}

	stop := func(sig os.Signal) {
		slog.Info("received signal, shutting down...", "signal", sig.String())

		ipscServer.Stop()
		for _, client := range hbrpClients {
			client.Stop()
		}
	}

	shutdown.AddWithParam(stop)
	shutdown.Listen(syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)

	return nil
}
