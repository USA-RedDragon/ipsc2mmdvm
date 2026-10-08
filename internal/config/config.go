package config

import (
	"errors"
	"net"
	"regexp"

	"github.com/vishvananda/netlink"
)

//go:generate go tool configulator -type Config

type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

type Config struct {
	LogLevel LogLevel `name:"log-level" description:"Logging level for the application. One of debug, info, warn, or error" default:"info"`
	Metrics  Metrics  `name:"metrics" description:"Configuration for Prometheus metrics"`
	MMDVM    []MMDVM  `name:"mmdvm" description:"Configuration for MMDVM clients (multiple DMR masters)"`
	IPSC     IPSC     `name:"ipsc" description:"Configuration for the IPSC server"`
}

type Metrics struct {
	Enabled bool   `name:"enabled" description:"Whether to enable Prometheus metrics endpoint"`
	Address string `name:"address" description:"Address to serve Prometheus metrics on" default:":9100"`
}

// IPSC creates a virtual network interface and listens for IPSC packets on it.
type IPSC struct {
	Interface  string   `name:"interface" description:"Interface to listen for IPSC packets on"`
	Port       uint16   `name:"port" description:"Port to listen for IPSC packets on"`
	IP         string   `name:"ip" description:"IP address to listen for IPSC packets on" default:"10.10.250.1"`
	SubnetMask int      `name:"subnet-mask" description:"Subnet mask for the virtual network interface created for IPSC packets" default:"24"`
	Auth       IPSCAuth `name:"auth" description:"Authentication configuration for the IPSC server"`
}

type IPSCAuth struct {
	Enabled bool   `name:"enabled" description:"Whether to require authentication for IPSC clients"`
	Key     string `name:"key" description:"Authentication key for IPSC clients. Required if auth is enabled"`
}

type MMDVM struct {
	Name     string `name:"name" description:"Name for this MMDVM network (used in logging)"`
	Callsign string `name:"callsign" description:"Callsign to use for the MMDVM connection"`
	ID       uint32 `name:"radio-id" description:"Radio ID for the MMDVM connection"`
	// RXFreq is in Hz
	RXFreq uint `name:"rx-freq" description:"Receive frequency in Hz for the MMDVM connection"`
	// TXFreq is in Hz
	TXFreq uint `name:"tx-freq" description:"Transmit frequency in Hz for the MMDVM connection"`
	// TXPower is in dBm
	TXPower uint8 `name:"tx-power" description:"Transmit power in dBm for the MMDVM connection"`
	// ColorCode is the DMR color code
	ColorCode uint8 `name:"color-code" description:"DMR color code for the MMDVM connection"`
	// Latitude with north as positive [-90,+90]
	Latitude float64 `name:"latitude" description:"Latitude with north as positive [-90,+90] for the MMDVM connection"`
	// Longitude with east as positive [-180+,180]
	Longitude float64 `name:"longitude" description:"Longitude with east as positive [-180+,180] for the MMDVM connection"`
	// Height in meters
	Height       uint16 `name:"height" description:"Height in meters for the MMDVM connection"`
	Location     string `name:"location" description:"Location for the MMDVM connection"`
	Description  string `name:"description" description:"Description for the MMDVM connection"`
	URL          string `name:"url" description:"URL for the MMDVM connection"`
	Slots        byte   `name:"slots" description:"Active timeslots bitmask (1=TS1, 2=TS2, 3=both)" default:"3"`
	MasterServer string `name:"master-server" description:"Master server for the MMDVM connection"`
	Password     string `name:"password" description:"Password for the MMDVM connection"`

	// Rewrite rules for routing DMR data to/from this network.
	TGRewrites   []TGRewriteConfig   `name:"tg-rewrite" description:"Talkgroup rewrite rules"`
	PCRewrites   []PCRewriteConfig   `name:"pc-rewrite" description:"Private call rewrite rules"`
	TypeRewrites []TypeRewriteConfig `name:"type-rewrite" description:"Type rewrite rules (group TG to private call)"`
	SrcRewrites  []SrcRewriteConfig  `name:"src-rewrite" description:"Source rewrite rules (private call by source to group TG)"`

	// PassAll rules allow all traffic of a given type on a slot without rewriting.
	PassAllPC []int `name:"pass-all-pc" description:"Timeslots on which all private calls pass through unchanged (e.g. [1, 2])"`
	PassAllTG []int `name:"pass-all-tg" description:"Timeslots on which all group calls pass through unchanged (e.g. [1, 2])"`
}

// TGRewriteConfig maps group TG calls from one slot/TG to another.
// Modeled after DMRGateway's TGRewrite: fromSlot, fromTG, toSlot, toTG, range.
type TGRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromTG   uint `name:"from-tg" description:"Source talkgroup start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToTG     uint `name:"to-tg" description:"Destination talkgroup start"`
	Range    uint `name:"range" description:"Number of contiguous TGs to map" default:"1"`
}

// PCRewriteConfig maps private calls from one slot/ID to another.
// Modeled after DMRGateway's PCRewrite: fromSlot, fromId, toSlot, toId, range.
type PCRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromID   uint `name:"from-id" description:"Source private call ID start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" description:"Destination private call ID start"`
	Range    uint `name:"range" description:"Number of contiguous IDs to map" default:"1"`
}

// TypeRewriteConfig converts group TG calls to private calls.
// Modeled after DMRGateway's TypeRewrite: fromSlot, fromTG, toSlot, toId, range.
type TypeRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromTG   uint `name:"from-tg" description:"Source talkgroup start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" description:"Destination private call ID start"`
	Range    uint `name:"range" description:"Number of contiguous entries to map" default:"1"`
}

// SrcRewriteConfig matches calls by source ID and remaps the source into a prefixed range.
type SrcRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromID   uint `name:"from-id" description:"Source ID start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" description:"Destination source ID start"`
	Range    uint `name:"range" description:"Number of contiguous source IDs to match" default:"1"`
}

var (
	ErrInvalidLogLevel          = errors.New("invalid log level provided")
	ErrNoMMDVMNetworks          = errors.New("at least one MMDVM network must be configured")
	ErrInvalidMMDVMName         = errors.New("invalid MMDVM network name provided")
	ErrDuplicateMMDVMName       = errors.New("duplicate MMDVM network name provided")
	ErrInvalidMMDVMCallsign     = errors.New("invalid MMDVM callsign provided")
	ErrInvalidMMDVMColorCode    = errors.New("invalid MMDVM color code provided")
	ErrInvalidMMDVMLongitude    = errors.New("invalid MMDVM longitude provided")
	ErrInvalidMMDVMLatitude     = errors.New("invalid MMDVM latitude provided")
	ErrInvalidMMDVMMasterServer = errors.New("invalid MMDVM master server provided")
	ErrInvalidMMDVMPassword     = errors.New("invalid MMDVM password provided")
	ErrInvalidRewriteSlot       = errors.New("invalid rewrite slot (must be 1 or 2)")
	ErrInvalidRewriteRange      = errors.New("invalid rewrite range (must be >= 1)")
	ErrInvalidIPSCInterface     = errors.New("invalid IPSC interface provided")
	ErrInvalidIPSCIP            = errors.New("invalid IPSC IP address provided")
	ErrInvalidIPSCSubnetMask    = errors.New("invalid IPSC subnet mask provided")
	ErrInvalidIPSCAuthKey       = errors.New("invalid IPSC authentication key provided")
	ErrInvalidMetricsAddress    = errors.New("invalid metrics address provided")
)

func (c Config) Validate() error {
	switch c.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return ErrInvalidLogLevel
	}

	if c.Metrics.Enabled && c.Metrics.Address != "" {
		_, _, err := net.SplitHostPort(c.Metrics.Address)
		if err != nil {
			return ErrInvalidMetricsAddress
		}
	}

	if len(c.MMDVM) == 0 {
		return ErrNoMMDVMNetworks
	}

	names := make(map[string]struct{}, len(c.MMDVM))
	for i := range c.MMDVM {
		h := &c.MMDVM[i]

		// Default name to "Network N" if empty
		if h.Name == "" {
			return ErrInvalidMMDVMName
		}

		if _, ok := names[h.Name]; ok {
			return ErrDuplicateMMDVMName
		}
		names[h.Name] = struct{}{}

		if h.Callsign == "" {
			return ErrInvalidMMDVMCallsign
		}

		if h.ColorCode > 15 {
			return ErrInvalidMMDVMColorCode
		}

		if h.Longitude < -180 || h.Longitude > 180 {
			return ErrInvalidMMDVMLongitude
		}

		if h.Latitude < -90 || h.Latitude > 90 {
			return ErrInvalidMMDVMLatitude
		}

		if h.MasterServer == "" {
			return ErrInvalidMMDVMMasterServer
		}

		if h.Password == "" {
			return ErrInvalidMMDVMPassword
		}

		if err := validateRewrites(h); err != nil {
			return err
		}
	}

	if c.IPSC.Interface == "" {
		return ErrInvalidIPSCInterface
	}

	_, err := netlink.LinkByName(c.IPSC.Interface)
	if err != nil {
		return ErrInvalidIPSCInterface
	}

	if c.IPSC.IP == "" {
		return ErrInvalidIPSCIP
	}

	if c.IPSC.SubnetMask < 1 || c.IPSC.SubnetMask > 32 {
		return ErrInvalidIPSCSubnetMask
	}

	if c.IPSC.Auth.Enabled && c.IPSC.Auth.Key == "" {
		return ErrInvalidIPSCAuthKey
	}

	// Check authkey is [0-9a-fA-F]{0,40} if c.IPSC.Auth.Enabled {
	regexp := regexp.MustCompile(`^[0-9a-fA-F]{0,40}$`)
	if !regexp.MatchString(c.IPSC.Auth.Key) {
		return ErrInvalidIPSCAuthKey
	}

	return nil
}

func validateSlot(slot uint) bool {
	return slot == 1 || slot == 2
}

func validateRewrites(h *MMDVM) error {
	for _, r := range h.TGRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.PCRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.TypeRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.SrcRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	return nil
}
