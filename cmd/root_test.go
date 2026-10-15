package cmd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

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
