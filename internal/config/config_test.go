package config

import (
	"errors"
	"testing"
)

// validConfig returns a minimal Config that passes all validation checks
// that don't depend on OS state (netlink). Because Validate() calls
// netlink.LinkByName we can only exercise the checks that run *before*
// the interface lookup or the auth-key regex check.
func validConfig() Config {
	return Config{
		LogLevel: LogLevelInfo,
		HBRP: []HBRP{
			{
				Name:         "BM",
				Callsign:     "N0CALL",
				ID:           12345,
				ColorCode:    7,
				Latitude:     30.0,
				Longitude:    -97.0,
				MasterServer: "master.example.com:62030",
				Password:     "password",
			},
		},
		IPSC: IPSC{
			Interface:  "lo", // loopback exists on all Linux hosts
			Port:       50000,
			IP:         "10.10.250.1",
			SubnetMask: 24,
			Auth: IPSCAuth{
				Enabled: false,
			},
		},
	}
}

func TestValidateLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		level    LogLevel
		wantErr  error
		hasError bool
	}{
		{"debug", LogLevelDebug, nil, false},
		{"info", LogLevelInfo, nil, false},
		{"warn", LogLevelWarn, nil, false},
		{"error", LogLevelError, nil, false},
		{"invalid", LogLevel("trace"), ErrInvalidLogLevel, true},
		{"empty", LogLevel(""), ErrInvalidLogLevel, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.LogLevel = tt.level
			err := c.Validate()
			if tt.hasError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
			}
			// Valid levels may still fail on netlink lookup,
			// so we only assert that the log-level error is NOT returned.
			if !tt.hasError && errors.Is(err, ErrInvalidLogLevel) {
				t.Fatalf("did not expect %v, got %v", ErrInvalidLogLevel, err)
			}
		})
	}
}

func TestValidateHBRPCallsign(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.HBRP[0].Callsign = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidHBRPCallsign) {
		t.Fatalf("expected %v, got %v", ErrInvalidHBRPCallsign, err)
	}
}

func TestValidateHBRPColorCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cc      uint8
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid 15", 15, false},
		{"invalid 16", 16, true},
		{"invalid 255", 255, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.HBRP[0].ColorCode = tt.cc
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidHBRPColorCode) {
				t.Fatalf("expected %v, got %v", ErrInvalidHBRPColorCode, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidHBRPColorCode) {
				t.Fatalf("did not expect %v", ErrInvalidHBRPColorCode)
			}
		})
	}
}

func TestValidateHBRPLatitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lat     float64
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid -90", -90, false},
		{"valid 90", 90, false},
		{"invalid -91", -91, true},
		{"invalid 91", 91, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.HBRP[0].Latitude = tt.lat
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidHBRPLatitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidHBRPLatitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidHBRPLatitude) {
				t.Fatalf("did not expect %v", ErrInvalidHBRPLatitude)
			}
		})
	}
}

func TestValidateHBRPLongitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lng     float64
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid -180", -180, false},
		{"valid 180", 180, false},
		{"invalid -181", -181, true},
		{"invalid 181", 181, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.HBRP[0].Longitude = tt.lng
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidHBRPLongitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidHBRPLongitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidHBRPLongitude) {
				t.Fatalf("did not expect %v", ErrInvalidHBRPLongitude)
			}
		})
	}
}

func TestValidateHBRPMasterServer(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.HBRP[0].MasterServer = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidHBRPMasterServer) {
		t.Fatalf("expected %v, got %v", ErrInvalidHBRPMasterServer, err)
	}
}

func TestValidateHBRPPassword(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.HBRP[0].Password = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidHBRPPassword) {
		t.Fatalf("expected %v, got %v", ErrInvalidHBRPPassword, err)
	}
}

func TestValidateIPSCInterface(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Interface = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCInterface) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCInterface, err)
	}
}

func TestValidateIPSCSubnetMask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mask    int
		wantErr bool
	}{
		{"valid 1", 1, false},
		{"valid 24", 24, false},
		{"valid 32", 32, false},
		{"invalid 0", 0, true},
		{"invalid 33", 33, true},
		{"invalid -1", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.IPSC.SubnetMask = tt.mask
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidIPSCSubnetMask) {
				t.Fatalf("expected %v, got %v", ErrInvalidIPSCSubnetMask, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidIPSCSubnetMask) {
				t.Fatalf("did not expect %v", ErrInvalidIPSCSubnetMask)
			}
		})
	}
}

func TestValidateIPSCAuthKeyRequired(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCAuthKey, err)
	}
}

func TestValidateIPSCAuthKeyBadHex(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = "ZZZZ" // Not valid hex
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCAuthKey, err)
	}
}

func TestValidateIPSCAuthKeyValid(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = "deadbeef"
	err := c.Validate()
	// Should not fail on auth key validation itself
	if errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("did not expect %v", ErrInvalidIPSCAuthKey)
	}
}

func TestLogLevelConstants(t *testing.T) {
	t.Parallel()
	if LogLevelDebug != "debug" {
		t.Fatalf("expected 'debug', got %q", LogLevelDebug)
	}
	if LogLevelInfo != "info" {
		t.Fatalf("expected 'info', got %q", LogLevelInfo)
	}
	if LogLevelWarn != "warn" {
		t.Fatalf("expected 'warn', got %q", LogLevelWarn)
	}
	if LogLevelError != "error" {
		t.Fatalf("expected 'error', got %q", LogLevelError)
	}
}
