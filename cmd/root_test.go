package cmd

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

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
