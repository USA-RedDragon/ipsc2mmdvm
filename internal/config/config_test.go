package config

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

const validZero = "valid 0"

// validConfig returns a minimal Config that passes all validation checks
// that don't depend on OS state (netlink). Because Validate() calls
// netlink.LinkByName we can only exercise the checks that run *before*
// the interface lookup or the auth-key regex check.
func validConfig() Config {
	return Config{
		LogLevel: LogLevelInfo,
		MMDVM: []MMDVM{
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

func TestValidateMMDVMCallsign(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVM[0].Callsign = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMCallsign) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMCallsign, err)
	}
}

func TestValidateMMDVMColorCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cc      uint8
		wantErr bool
	}{
		{validZero, 0, false},
		{"valid 15", 15, false},
		{"invalid 16", 16, true},
		{"invalid 255", 255, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVM[0].ColorCode = tt.cc
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMColorCode) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMColorCode, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMColorCode) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMColorCode)
			}
		})
	}
}

func TestValidateMMDVMLatitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lat     float64
		wantErr bool
	}{
		{validZero, 0, false},
		{"valid -90", -90, false},
		{"valid 90", 90, false},
		{"invalid -91", -91, true},
		{"invalid 91", 91, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVM[0].Latitude = tt.lat
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMLatitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMLatitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMLatitude) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMLatitude)
			}
		})
	}
}

func TestValidateMMDVMLongitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lng     float64
		wantErr bool
	}{
		{validZero, 0, false},
		{"valid -180", -180, false},
		{"valid 180", 180, false},
		{"invalid -181", -181, true},
		{"invalid 181", 181, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVM[0].Longitude = tt.lng
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMLongitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMLongitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMLongitude) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMLongitude)
			}
		})
	}
}

func TestValidateMMDVMMasterServer(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVM[0].MasterServer = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMMasterServer) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMMasterServer, err)
	}
}

func TestValidateMMDVMPassword(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVM[0].Password = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMPassword) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMPassword, err)
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

func TestValidateMetricsAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"default port only", ":9100", false},
		{"host and port", "0.0.0.0:9100", false},
		{"localhost", "127.0.0.1:2112", false},
		{"named port", ":http", false},
		{"host and named port", "127.0.0.1:https", false},
		{"empty", "", true},
		{"missing port", "localhost", true},
		{"invalid format", ":::bad", true},
		{"non-numeric port", "nohost:notaport", true},
		{"port out of range", ":99999", true},
		{"port zero", ":0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.Metrics.Enabled = true
			c.Metrics.Address = tt.addr
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMetricsAddress) {
				t.Fatalf("expected %v, got %v", ErrInvalidMetricsAddress, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMetricsAddress) {
				t.Fatalf("did not expect %v, got %v", ErrInvalidMetricsAddress, err)
			}
		})
	}
}

func TestValidateMetricsAddressErrorNamesTheProblem(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.Metrics.Enabled = true
	c.Metrics.Address = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "metrics.address is empty") {
		t.Fatalf("error %v does not say that metrics.address is empty", err)
	}
	c.Metrics.Address = "metrics.example"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `"metrics.example"`) {
		t.Fatalf("error %v does not name the bad address", err)
	}
}

func TestValidConfigPasses(t *testing.T) {
	t.Parallel()
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
}

func TestValidateFieldLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr error
	}{
		{"callsign 8 chars", func(c *Config) { c.MMDVM[0].Callsign = "AB1CDEFG" }, nil},
		{"callsign 9 chars", func(c *Config) { c.MMDVM[0].Callsign = "AB1CDEFGH" }, nil},
		{"radio id zero", func(c *Config) { c.MMDVM[0].ID = 0 }, ErrInvalidMMDVMRadioID},
		{"radio id 9 digits", func(c *Config) { c.MMDVM[0].ID = 999999999 }, nil},
		{"radio id 10 digits", func(c *Config) { c.MMDVM[0].ID = 1000000000 }, ErrInvalidMMDVMRadioID},
		{"rx freq 9 digits", func(c *Config) { c.MMDVM[0].RXFreq = 999999999 }, nil},
		{"rx freq 10 digits", func(c *Config) { c.MMDVM[0].RXFreq = 1000000000 }, ErrInvalidMMDVMFrequency},
		{"tx freq 10 digits", func(c *Config) { c.MMDVM[0].TXFreq = 4294967295 }, ErrInvalidMMDVMFrequency},
		{"tx power 99", func(c *Config) { c.MMDVM[0].TXPower = 99 }, nil},
		{"tx power 100", func(c *Config) { c.MMDVM[0].TXPower = 100 }, ErrInvalidMMDVMTXPower},
		{"height 999", func(c *Config) { c.MMDVM[0].Height = 999 }, nil},
		{"height 1000", func(c *Config) { c.MMDVM[0].Height = 1000 }, ErrInvalidMMDVMHeight},
		{"location 20", func(c *Config) { c.MMDVM[0].Location = strings.Repeat("a", 20) }, nil},
		{"location 21", func(c *Config) { c.MMDVM[0].Location = "Springfield, Illinois, USA" }, nil},
		{"description 19", func(c *Config) { c.MMDVM[0].Description = strings.Repeat("a", 19) }, nil},
		{"description 20", func(c *Config) { c.MMDVM[0].Description = strings.Repeat("a", 20) }, nil},
		{"url 124", func(c *Config) { c.MMDVM[0].URL = strings.Repeat("a", 124) }, nil},
		{"url 125", func(c *Config) { c.MMDVM[0].URL = strings.Repeat("a", 125) }, nil},
		{"slots 4", func(c *Config) { c.MMDVM[0].Slots = 4 }, nil},
		{"slots 5", func(c *Config) { c.MMDVM[0].Slots = 5 }, ErrInvalidMMDVMSlots},
		{"latitude NaN", func(c *Config) { c.MMDVM[0].Latitude = math.NaN() }, ErrInvalidMMDVMLatitude},
		{"longitude NaN", func(c *Config) { c.MMDVM[0].Longitude = math.NaN() }, ErrInvalidMMDVMLongitude},
		{"pass-all-tg slot 2", func(c *Config) { c.MMDVM[0].PassAllTG = []int{1, 2} }, nil},
		{"pass-all-tg slot 5", func(c *Config) { c.MMDVM[0].PassAllTG = []int{5} }, ErrInvalidPassAllSlot},
		{"pass-all-pc slot 0", func(c *Config) { c.MMDVM[0].PassAllPC = []int{0} }, ErrInvalidPassAllSlot},
		{"ipsc port zero", func(c *Config) { c.IPSC.Port = 0 }, ErrInvalidIPSCPort},
		{"call hang hold negative", func(c *Config) { c.IPSC.CallHangHold = -time.Second }, ErrInvalidCallHangHold},
		{"call hang hold too long", func(c *Config) { c.IPSC.CallHangHold = MaxCallHangHold + time.Millisecond }, ErrInvalidCallHangHold},
		{"call hang hold off", func(c *Config) { c.IPSC.CallHangHold = 0 }, nil},
		{"call hang hold at the cap", func(c *Config) { c.IPSC.CallHangHold = MaxCallHangHold }, nil},
		{"ipsc ip empty", func(c *Config) { c.IPSC.IP = "" }, ErrInvalidIPSCIP},
		{"ipsc ip garbage", func(c *Config) { c.IPSC.IP = "not-an-ip" }, ErrInvalidIPSCIP},
		{"ipsc ip with mask", func(c *Config) { c.IPSC.IP = "10.10.250.1/24" }, ErrInvalidIPSCIP},
		{"ipsc ip ipv6", func(c *Config) { c.IPSC.IP = "fe80::1" }, ErrInvalidIPSCIP},
		{"ipsc ip v4-mapped", func(c *Config) { c.IPSC.IP = "::ffff:10.10.250.1" }, ErrInvalidIPSCIP},
		{"ipsc ip unspecified", func(c *Config) { c.IPSC.IP = "0.0.0.0" }, ErrInvalidIPSCIP},
		{"ipsc ip broadcast", func(c *Config) { c.IPSC.IP = "255.255.255.255" }, ErrInvalidIPSCIP},
		{"ipsc ip multicast", func(c *Config) { c.IPSC.IP = "224.0.0.1" }, ErrInvalidIPSCIP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("expected valid config, got %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateSrcRewriteRequiresTG(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVM[0].SrcRewrites = []SrcRewriteConfig{{FromSlot: 1, FromID: 1234, ToSlot: 2, ToTG: 0, Range: 1}}
	err := c.Validate()
	if !errors.Is(err, ErrInvalidRewriteTG) {
		t.Fatalf("expected %v, got %v", ErrInvalidRewriteTG, err)
	}
	if !strings.Contains(err.Error(), "src-rewrite: to-id was renamed to to-tg") {
		t.Fatalf("error %q does not name the to-id rename", err)
	}
	if !strings.Contains(err.Error(), "matches only private calls") || !strings.Contains(err.Error(), "group calls to to-tg") {
		t.Fatalf("error %q does not explain the new src-rewrite meaning", err)
	}
	c.MMDVM[0].SrcRewrites[0].ToTG = 9
	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
}
