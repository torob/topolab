// Package network owns the Linux host datapath used by TopoLab.
//
// The package deliberately accepts a small, normalized networking model rather
// than the YAML model.  This keeps command execution injectable and makes the
// network lifecycle usable by reconciliation without importing CLI concerns.
package network

import (
	"context"
	"encoding/json"
	"time"
)

const DatapathVersion = 2

// CommandRunner executes one program directly. Implementations must not invoke
// a shell. Output is the command's combined stdout and stderr.
type CommandRunner interface {
	Run(ctx context.Context, program string, args ...string) ([]byte, error)
}

// HostNetwork is the injectable host-network lifecycle used by reconciliation.
type HostNetwork interface {
	Plan(context.Context, Config) (*Plan, error)
	Apply(context.Context, *Plan) (*State, error)
	Verify(context.Context, *State) error
	Remove(context.Context, *State) error
	Counters(context.Context, *State) (*CounterSnapshot, error)
}

type EndpointKind string

const (
	EndpointVM             EndpointKind = "vm"
	EndpointRouter         EndpointKind = "router"
	EndpointManagementHost EndpointKind = "management-host"
	EndpointSwitch         EndpointKind = "switch"
)

// Config is fully normalized: MTUs, access VLAN 1, and queue count 1 should be
// explicit before it reaches this package.
type Config struct {
	Topology            string
	SharedDir           string
	Switches            []Switch
	Routers             []Router
	Links               []Link
	Externals           []External
	ExistingOwnedRoutes []OwnedRoute
}

// OwnedRoute identifies one exact route recorded by the current topology
// manifest. It is preflight context, not desired network configuration.
type OwnedRoute struct{ Prefix, Device, Via, Protocol, Marker string }

type Switch struct {
	Name          string
	MTU           int
	STP           bool
	VLANFiltering bool
}

type Router struct {
	Name       string
	Forwarding bool
	Interfaces []RouterInterface
	Routes     []Route
	NAT        []NAT
	DHCP       []DHCPServer
}

// RouterInterface describes the continuously managed side inside a namespace.
// The host fabric side is derived from the link endpoint.
type RouterInterface struct {
	Name      string
	MTU       int
	MAC       string
	Addresses []string
	VLANs     []VLAN
}

type VLAN struct {
	ID        int
	MTU       int
	Addresses []string
}

type Route struct {
	To        string
	Via       string
	Interface string
}

type NAT struct {
	Interface string
	Sources   []string
}

type DHCPServer struct {
	Interface  string
	RangeStart string
	RangeEnd   string
	Lease      string
	DNSServers []string
}

type Link struct {
	Name      string
	MTU       int
	Endpoints [2]Endpoint
}

type Endpoint struct {
	Node      string
	Interface string
	Kind      EndpointKind
	MTU       int
	MAC       string
	Queues    int
	Addresses []string // management-host addresses; router addresses come from Routers
	Port      Port
	// VLANFiltering mirrors the switch setting. It is set only on switch endpoints.
	VLANFiltering bool
	Egress        Impairment
}

type Port struct {
	Mode         string
	VLAN         int
	AllowedVLANs []int
	NativeVLAN   int
}

type Impairment struct {
	LatencyMS   float64
	JitterMS    float64
	LossPercent float64
	RateMbps    float64
}

// External is a fully resolved external-router transit. Transit allocation and
// physical-egress discovery are preflight responsibilities of Plan.
type External struct {
	Router          string
	Interface       string
	MAC             string
	MTU             int
	EgressInterface string // may be "auto" during Plan
	SourceIP        string // may be "auto" during Plan
	Sources         []string
	TransitPrefix   string // empty requests deterministic allocation
	RouterAddress   string // derived from TransitPrefix when empty
	HostAddress     string // derived from TransitPrefix when empty
}

type Plan struct {
	Config   Config
	External []ResolvedExternal
}

type ResolvedExternal struct {
	External
	Masquerade bool
}

type ResourceKind string

const (
	ResourceLink      ResourceKind = "link"
	ResourceBridge    ResourceKind = "bridge"
	ResourceTAP       ResourceKind = "tap"
	ResourceNamespace ResourceKind = "namespace"
	ResourceNFTTable  ResourceKind = "nft-table"
	ResourceProcess   ResourceKind = "process"
)

type Resource struct {
	Kind      ResourceKind `json:"kind"`
	Name      string       `json:"name"`
	Namespace string       `json:"namespace,omitempty"`
	Alias     string       `json:"alias,omitempty"`
	MTU       int          `json:"mtu,omitempty"`
}

// State contains only records needed to validate ownership, clean up, report
// status, launch QEMU, and collect counters. It is safe to persist as JSON.
type State struct {
	DatapathVersion          int                                `json:"datapath_version"`
	Topology                 string                             `json:"topology"`
	Resources                []Resource                         `json:"resources"`
	Links                    map[string]LinkState               `json:"links"`
	TAPs                     map[string]TAPState                `json:"taps,omitempty"`
	ManagementHostInterfaces map[string]ManagementHostInterface `json:"management_host_interfaces,omitempty"`
	Namespaces               map[string]string                  `json:"namespaces,omitempty"`
	NamespaceAliases         map[string]string                  `json:"namespace_aliases,omitempty"`
	FabricNamespace          string                             `json:"fabric_namespace"`
	ManagementHostNamespace  string                             `json:"management_host_namespace"`
	External                 []ResolvedExternal                 `json:"external,omitempty"`
	RootNFTTable             string                             `json:"root_nft_table,omitempty"`
	FabricNFTTable           string                             `json:"fabric_nft_table,omitempty"`
	ManagementHostNFTTable   string                             `json:"management_host_nft_table,omitempty"`
	RouterNFTTables          map[string]string                  `json:"router_nft_tables,omitempty"`
	ForwardingOwner          bool                               `json:"forwarding_owner,omitempty"`
	ForwardingGuard          bool                               `json:"forwarding_guard,omitempty"`
	FirewallRules            []FirewallRule                     `json:"firewall_rules,omitempty"`
	SharedDir                string                             `json:"shared_dir,omitempty"`
}

// FirewallRule records a narrowly scoped accept rule inserted at the start of
// a pre-existing host forward hook. Such rules are required because an accept
// verdict in TopoLab's earlier base chain can still be dropped by a later base
// chain with a drop policy.
type FirewallRule struct {
	Family  string `json:"family"`
	Table   string `json:"table"`
	Chain   string `json:"chain"`
	Comment string `json:"comment"`
}

type TAPState struct {
	Node      string `json:"node"`
	Interface string `json:"interface"`
	Name      string `json:"name"`
	Queues    int    `json:"queues"`
	Alias     string `json:"alias"`
}

type ManagementHostInterface struct {
	LogicalName string   `json:"logical_name"`
	DeviceName  string   `json:"device_name"`
	Alias       string   `json:"alias"`
	MTU         int      `json:"mtu"`
	Addresses   []string `json:"addresses"`
	Link        string   `json:"link"`
}

type LinkState struct {
	Name                string          `json:"name"`
	Bridge              string          `json:"bridge"`
	GenerationID        string          `json:"generation_id"`
	GenerationStartedAt time.Time       `json:"generation_started_at"`
	Endpoints           []EndpointState `json:"endpoints"`
}

type EndpointState struct {
	Node             string `json:"node"`
	Interface        string `json:"interface"`
	IngressDevice    string `json:"ingress_device"`
	EgressDevice     string `json:"egress_device"`
	ImpairmentDevice string `json:"impairment_device"`
}

type CounterSnapshot struct {
	Topology  string        `json:"topology"`
	Collected time.Time     `json:"collected_at"`
	Links     []LinkCounter `json:"links"`
}

type LinkCounter struct {
	Name                string            `json:"name"`
	GenerationID        string            `json:"generation_id"`
	GenerationStartedAt time.Time         `json:"generation_started_at"`
	Endpoints           []EndpointCounter `json:"endpoints"`
}

type EndpointCounter struct {
	Node       string             `json:"node"`
	Interface  string             `json:"interface"`
	Ingress    TrafficCounters    `json:"ingress"`
	Egress     TrafficCounters    `json:"egress"`
	Impairment ImpairmentCounters `json:"impairment"`
	Device     DeviceCounters     `json:"device"`
}

type TrafficCounters struct {
	Packets *uint64 `json:"packets"`
	Bytes   *uint64 `json:"bytes"`
}

type ImpairmentCounters struct {
	OfferedPackets        *uint64 `json:"offered_packets"`
	OfferedBytes          *uint64 `json:"offered_bytes"`
	DeliveredPackets      *uint64 `json:"delivered_packets"`
	DeliveredBytes        *uint64 `json:"delivered_bytes"`
	ConfiguredLossDrops   *uint64 `json:"configured_loss_drops"`
	NetemQueueDrops       *uint64 `json:"netem_queue_drops"`
	RateLimiterDrops      *uint64 `json:"rate_limiter_drops"`
	RateLimiterOverlimits *uint64 `json:"rate_limiter_overlimits"`
	RateLimiterRequeues   *uint64 `json:"rate_limiter_requeues"`
	BacklogPackets        *uint64 `json:"backlog_packets"`
	BacklogBytes          *uint64 `json:"backlog_bytes"`
}

type DeviceCounters struct {
	RXErrors  *uint64 `json:"rx_errors"`
	RXDropped *uint64 `json:"rx_dropped"`
	TXErrors  *uint64 `json:"tx_errors"`
	TXDropped *uint64 `json:"tx_dropped"`
	MTUDrops  *uint64 `json:"mtu_drops"`
}

// MarshalState is convenient for manifest adapters which retain the network
// state as an opaque versioned field.
func MarshalState(s *State) (json.RawMessage, error) { return json.Marshal(s) }
