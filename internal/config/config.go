package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"regexp"
	"time"

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
	Port       uint16   `name:"port" description:"Port to listen for IPSC packets on" default:"50000"`
	IP         string   `name:"ip" description:"IP address to listen for IPSC packets on" default:"10.10.250.1"`
	SubnetMask int      `name:"subnet-mask" description:"Subnet mask for the virtual network interface created for IPSC packets" default:"24"`
	Auth       IPSCAuth `name:"auth" description:"Authentication configuration for the IPSC server"`
	// CallHangHold is how long after a voice call to IPSC ends on a
	// timeslot that data and CSBK calls to IPSC wait. An XPR 8400 drops
	// the data and CSBK calls it gets during its call hang, about 2 s.
	CallHangHold time.Duration `name:"call-hang-hold" description:"Wait for data and CSBK calls to IPSC after a voice call on a slot; 0 is off" default:"2.5s"`
}

// MaxCallHangHold is the longest ipsc.call-hang-hold that Validate accepts.
const MaxCallHangHold = 10 * time.Second

type IPSCAuth struct {
	Enabled bool   `name:"enabled" description:"Whether to require authentication for IPSC clients"`
	Key     string `name:"key" description:"Authentication key for IPSC clients. Required if auth is enabled" secret:"true"`
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
	Password     string `name:"password" description:"Password for the MMDVM connection" secret:"true"`

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

// SrcRewriteConfig converts private calls from a range of source IDs into
// group calls to one talkgroup. The source ID is not changed.
// Modeled after DMRGateway's SrcRewrite: fromSlot, fromId, toSlot, toTG, range.
type SrcRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromID   uint `name:"from-id" description:"Source ID start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToTG     uint `name:"to-tg" description:"Destination talkgroup for the converted group call"`
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
	ErrInvalidMMDVMRadioID      = errors.New("invalid MMDVM radio ID provided (must be 1-999999999)")
	ErrInvalidMMDVMFrequency    = errors.New("invalid MMDVM frequency provided (must be at most 9 digits)")
	ErrInvalidMMDVMTXPower      = errors.New("invalid MMDVM tx power provided (must be 0-99)")
	ErrInvalidMMDVMHeight       = errors.New("invalid MMDVM height provided (must be 0-999)")
	ErrInvalidMMDVMSlots        = errors.New("invalid MMDVM slots provided (must be 0-4)")
	ErrInvalidPassAllSlot       = errors.New("invalid pass-all slot (must be 1 or 2)")
	ErrInvalidRewriteSlot       = errors.New("invalid rewrite slot (must be 1 or 2)")
	ErrInvalidRewriteRange      = errors.New("invalid rewrite range (must be >= 1)")
	ErrInvalidRewriteTG         = errors.New("invalid rewrite talkgroup (must be >= 1)")
	ErrInvalidIPSCInterface     = errors.New("invalid IPSC interface provided")
	ErrInvalidIPSCPort          = errors.New("invalid IPSC port provided")
	ErrInvalidCallHangHold      = errors.New("invalid ipsc.call-hang-hold: want a duration from 0 to 10s")
	ErrInvalidIPSCIP            = errors.New("invalid IPSC IP address provided (must be a unicast IPv4 address)")
	ErrInvalidIPSCSubnetMask    = errors.New("invalid IPSC subnet mask provided")
	ErrInvalidIPSCAuthKey       = errors.New("invalid IPSC authentication key provided")
	ErrInvalidMetricsAddress    = errors.New("invalid metrics address provided")
)

// Numeric field limits of the RPTC configuration message, from the format
// string in DMRGateway's CDMRGateway::getConfig. Longer string fields are
// truncated when the message is built, as DMRGateway does:
// "%-8.8s%09u%09u%02u%02u%8.8s%9.9s%03d%-20.20s%-19.19s%c%-124.124s...".
const (
	maxFrequency   = 999999999
	maxTXPower     = 99
	maxHeight      = 999
	maxSlots       = 4
	latitudeWidth  = 8
	longitudeWidth = 9
	// maxRadioID allows 9-digit IDs, such as a 7-digit DMR ID with a
	// 2-digit hotspot suffix.
	maxRadioID = 999999999
)

func (c Config) Validate() error {
	switch c.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return ErrInvalidLogLevel
	}

	if c.Metrics.Enabled {
		if c.Metrics.Address == "" {
			return fmt.Errorf("%w: metrics.enabled is true but metrics.address is empty; set it to a listen address such as \":9100\"", ErrInvalidMetricsAddress)
		}
		if !validListenAddress(c.Metrics.Address) {
			return fmt.Errorf("%w %q: want [host]:port with a port from 1 to 65535", ErrInvalidMetricsAddress, c.Metrics.Address)
		}
	}

	if len(c.MMDVM) == 0 {
		return ErrNoMMDVMNetworks
	}

	names := make(map[string]struct{}, len(c.MMDVM))
	for i := range c.MMDVM {
		h := &c.MMDVM[i]

		if h.Name == "" {
			return ErrInvalidMMDVMName
		}

		if _, ok := names[h.Name]; ok {
			return ErrDuplicateMMDVMName
		}
		names[h.Name] = struct{}{}

		if err := validateMMDVM(h); err != nil {
			return err
		}
	}

	if c.IPSC.Interface == "" {
		return ErrInvalidIPSCInterface
	}

	if c.IPSC.Port == 0 {
		return ErrInvalidIPSCPort
	}

	if c.IPSC.CallHangHold < 0 || c.IPSC.CallHangHold > MaxCallHangHold {
		return ErrInvalidCallHangHold
	}

	if !validIPv4Unicast(c.IPSC.IP) {
		return ErrInvalidIPSCIP
	}

	_, err := netlink.LinkByName(c.IPSC.Interface)
	if err != nil {
		return ErrInvalidIPSCInterface
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

func validListenAddress(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	p, err := net.DefaultResolver.LookupPort(context.Background(), "tcp", port)
	return err == nil && p != 0
}

func validIPv4Unicast(s string) bool {
	ip, err := netip.ParseAddr(s)
	if err != nil || !ip.Is4() {
		return false
	}
	return !ip.IsUnspecified() && !ip.IsMulticast() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

func validateMMDVM(h *MMDVM) error {
	if h.Callsign == "" {
		return ErrInvalidMMDVMCallsign
	}

	if h.ID < 1 || h.ID > maxRadioID {
		return ErrInvalidMMDVMRadioID
	}

	if h.RXFreq > maxFrequency || h.TXFreq > maxFrequency {
		return ErrInvalidMMDVMFrequency
	}

	if h.TXPower > maxTXPower {
		return ErrInvalidMMDVMTXPower
	}

	if h.ColorCode > 15 {
		return ErrInvalidMMDVMColorCode
	}

	if math.IsNaN(h.Latitude) || h.Latitude < -90 || h.Latitude > 90 ||
		len(fmt.Sprintf("%+08.4f", h.Latitude)) != latitudeWidth {
		return ErrInvalidMMDVMLatitude
	}

	if math.IsNaN(h.Longitude) || h.Longitude < -180 || h.Longitude > 180 ||
		len(fmt.Sprintf("%+09.4f", h.Longitude)) != longitudeWidth {
		return ErrInvalidMMDVMLongitude
	}

	if h.Height > maxHeight {
		return ErrInvalidMMDVMHeight
	}

	if h.Slots > maxSlots {
		return ErrInvalidMMDVMSlots
	}

	if h.MasterServer == "" {
		return ErrInvalidMMDVMMasterServer
	}

	if h.Password == "" {
		return ErrInvalidMMDVMPassword
	}

	for _, s := range h.PassAllTG {
		if s != 1 && s != 2 {
			return ErrInvalidPassAllSlot
		}
	}
	for _, s := range h.PassAllPC {
		if s != 1 && s != 2 {
			return ErrInvalidPassAllSlot
		}
	}

	return validateRewrites(h)
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
		if r.ToTG < 1 {
			return fmt.Errorf("%w; src-rewrite: to-id was renamed to to-tg, and the rule changed: "+
				"it no longer rewrites the source ID of any call, it now matches only private calls "+
				"from the source IDs and turns them into group calls to to-tg, like DMRGateway SrcRewrite", ErrInvalidRewriteTG)
		}
	}
	return nil
}
