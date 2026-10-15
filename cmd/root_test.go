package cmd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestStartMetricsServerFailsWhenPortInUse(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv, err := startMetricsServer(context.Background(), ln.Addr().String(), http.NotFoundHandler())
	if err == nil {
		_ = srv.Close()
		t.Fatal("expected a bind error for an address in use")
	}
}

func TestStartMetricsServerServes(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	srv, err := startMetricsServer(context.Background(), addr, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("expected the server to be listening when start returns: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestWaitForShutdownRunsStop(t *testing.T) {
	t.Parallel()
	sigCh := make(chan os.Signal, 2)
	sigCh <- syscall.SIGTERM
	stopped := false
	if err := waitForShutdown(sigCh, func() { stopped = true }); err != nil {
		t.Fatalf("expected clean shutdown, got %v", err)
	}
	if !stopped {
		t.Fatal("expected stop to run")
	}
}

func TestWaitForShutdownSecondSignalForcesExit(t *testing.T) {
	t.Parallel()
	sigCh := make(chan os.Signal, 2)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})

	result := make(chan error, 1)
	go func() {
		result <- waitForShutdown(sigCh, func() {
			close(started)
			<-release
		})
	}()

	sigCh <- syscall.SIGINT
	<-started
	sigCh <- syscall.SIGINT

	select {
	case err := <-result:
		if !errors.Is(err, errForcedShutdown) {
			t.Fatalf("expected %v, got %v", errForcedShutdown, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second signal did not interrupt a hung shutdown")
	}
}

func TestRuntimeErrorDoesNotPrintUsage(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("test", "test")
	wantErr := errors.New("runtime failure")
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return wantErr }
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
	if bytes.Contains(out.Bytes(), []byte("Usage:")) {
		t.Fatalf("runtime error printed usage:\n%s", out.String())
	}
}
