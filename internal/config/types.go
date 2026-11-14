package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the normalized v2 topology document. Named collections are ordered
// slices; index is a disposable lookup accelerator built after decoding.
type Config struct {
	Name           string         `yaml:"name"`
	Templates      []*Template    `yaml:"templates"`
	Storages       []*Storage     `yaml:"storages"`
	ManagementHost ManagementHost `yaml:"management_host"`
	VMs            []*VM          `yaml:"vms"`
	Routers        []*Router      `yaml:"routers"`
	Switches       []*Switch      `yaml:"switches"`
	Links          []*Link        `yaml:"links"`
	index          *configIndex
}

// Storage is an explicitly selected disk backend. The private presence bits
// deliberately distinguish an omitted field from an explicitly supplied
// empty/zero field so type-specific validation never silently ignores input.
type Storage struct {
	Name             string `yaml:"name"`
	Type             string `yaml:"type"`
	VolumeGroup      string `yaml:"volume_group,omitempty"`
	PoolSizeGiB      uint64 `yaml:"pool_size_gib,omitempty"`
	RootProvisioning string `yaml:"root_provisioning,omitempty"`
	ZeroNewBlocks    bool   `yaml:"zero_new_blocks,omitempty"`
	WipeOnCreate     bool   `yaml:"wipe_on_create,omitempty"`

	volumeGroupSet      bool
	poolSizeGiBSet      bool
	rootProvisioningSet bool
	zeroNewBlocksSet    bool
	wipeOnCreateSet     bool
}

func (s *Storage) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("storage must be a mapping")
	}
	allowed := map[string]bool{"name": true, "type": true, "volume_group": true, "pool_size_gib": true, "root_provisioning": true, "zero_new_blocks": true, "wipe_on_create": true}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !allowed[key] {
			return fmt.Errorf("field %s not found in type config.Storage", key)
		}
		if seen[key] {
			return fmt.Errorf("storage field %q is duplicated", key)
		}
		seen[key] = true
		switch key {
		case "name":
			if err := node.Content[i+1].Decode(&s.Name); err != nil {
				return err
			}
		case "type":
			if err := node.Content[i+1].Decode(&s.Type); err != nil {
				return err
			}
		case "volume_group":
			s.volumeGroupSet = true
			if err := node.Content[i+1].Decode(&s.VolumeGroup); err != nil {
				return err
			}
		case "pool_size_gib":
			s.poolSizeGiBSet = true
			if err := node.Content[i+1].Decode(&s.PoolSizeGiB); err != nil {
				return err
			}
		case "root_provisioning":
			s.rootProvisioningSet = true
			if err := node.Content[i+1].Decode(&s.RootProvisioning); err != nil {
				return err
			}
		case "zero_new_blocks":
			s.zeroNewBlocksSet = true
			if err := node.Content[i+1].Decode(&s.ZeroNewBlocks); err != nil {
				return err
			}
		case "wipe_on_create":
			s.wipeOnCreateSet = true
			if err := node.Content[i+1].Decode(&s.WipeOnCreate); err != nil {
				return err
			}
		}
	}
	return nil
}

type Template struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type ManagementHost struct {
	Interfaces []*ManagementHostInterface `yaml:"interfaces"`
}

type ManagementHostInterface struct {
	Name string             `yaml:"name"`
	MTU  int                `yaml:"mtu"`
	IPv4 ManagementHostIPv4 `yaml:"ipv4"`
	MAC  string             `yaml:"-"`
}

type ManagementHostIPv4 struct {
	Addresses []string `yaml:"addresses"`
}

type VM struct {
	Name               string               `yaml:"name"`
	Template           string               `yaml:"template"`
	CPU                *CPU                 `yaml:"cpu,omitempty"`
	MemoryMB           int                  `yaml:"memory_mb"`
	MemoryBalloon      *MemoryBalloon       `yaml:"memory_balloon,omitempty"`
	RootDisk           RootDisk             `yaml:"root_disk"`
	AdditionalDisks    []*Disk              `yaml:"additional_disks"`
	GuestAgent         bool                 `yaml:"guest_agent"`
	Boot               Boot                 `yaml:"boot"`
	SSHAuthorizedKeys  []string             `yaml:"ssh_authorized_keys"`
	RootCACertificates []*RootCACertificate `yaml:"root_ca_certificates"`
	Interfaces         []*VMInterface       `yaml:"interfaces"`
	// StartDelay is how long to wait right before this VM starts, such as
	// "30s". Empty means no wait. Validation fills StartDelayDuration.
	StartDelay         string        `yaml:"start_delay,omitempty"`
	StartDelayDuration time.Duration `yaml:"-"`
	VCPUs              int           `yaml:"-"`
}

type RootCACertificate struct {
	PEM          string `yaml:"pem,omitempty"`
	Path         string `yaml:"path,omitempty"`
	CanonicalPEM string `yaml:"-" json:"-"`
}

type CPU struct {
	Model   string `yaml:"model"`
	Sockets *int   `yaml:"sockets,omitempty"`
	Cores   *int   `yaml:"cores,omitempty"`
	Threads *int   `yaml:"threads,omitempty"`
}

type MemoryBalloon struct {
	Enabled  bool `yaml:"enabled"`
	TargetMB *int `yaml:"target_mb,omitempty"`
}

type RootDisk struct {
	SizeGiB *uint64 `yaml:"size_gib,omitempty"`
	Bus     string  `yaml:"bus"`
	Storage string  `yaml:"storage"`
	Cache   string  `yaml:"cache"`
	AIO     string  `yaml:"aio"`
}

type Disk struct {
	Name    string `yaml:"name"`
	SizeGiB uint64 `yaml:"size_gib"`
	Bus     string `yaml:"bus"`
	Storage string `yaml:"storage"`
	Cache   string `yaml:"cache"`
	AIO     string `yaml:"aio"`
}

type Boot struct {
	Order  []string `yaml:"order"`
	Menu   bool     `yaml:"menu"`
	Strict bool     `yaml:"strict"`
}

type VMInterface struct {
	Name   string    `yaml:"name"`
	Model  string    `yaml:"model"`
	Queues int       `yaml:"queues"`
	MAC    string    `yaml:"mac"`
	MTU    int       `yaml:"mtu"`
	IPv4   *VMIPv4   `yaml:"ipv4,omitempty"`
	VLANs  []*VMVLAN `yaml:"vlans"`
}

type VMVLAN struct {
	ID   int     `yaml:"id"`
	MTU  int     `yaml:"mtu"`
	IPv4 *VMIPv4 `yaml:"ipv4,omitempty"`
}

type VMIPv4 struct {
	DHCP       bool      `yaml:"dhcp"`
	Addresses  []string  `yaml:"addresses"`
	Routes     []VMRoute `yaml:"routes"`
	DNSServers []string  `yaml:"dns_servers"`
}

type VMRoute struct {
	To  string `yaml:"to"`
	Via string `yaml:"via"`
}

type Router struct {
	Name       string             `yaml:"name"`
	Interfaces []*RouterInterface `yaml:"interfaces"`
	Routes     []RouterRoute      `yaml:"routes"`
}

type RouterInterface struct {
	Name     string        `yaml:"name"`
	MAC      string        `yaml:"mac"`
	MTU      int           `yaml:"mtu"`
	IPv4     *RouterIPv4   `yaml:"ipv4,omitempty"`
	VLANs    []*RouterVLAN `yaml:"vlans"`
	External *External     `yaml:"external,omitempty"`
	NAT      *NAT          `yaml:"nat,omitempty"`
}

type RouterVLAN struct {
	ID   int         `yaml:"id"`
	MTU  int         `yaml:"mtu"`
	IPv4 *RouterIPv4 `yaml:"ipv4,omitempty"`
}

type RouterIPv4 struct {
	Addresses  []string    `yaml:"addresses"`
	DHCPServer *DHCPServer `yaml:"dhcp_server,omitempty"`
}

type DHCPServer struct {
	RangeStart string   `yaml:"range_start"`
	RangeEnd   string   `yaml:"range_end"`
	Lease      string   `yaml:"lease"`
	DNSServers []string `yaml:"dns_servers"`
}

type External struct {
	Sources         []string `yaml:"sources"`
	EgressInterface string   `yaml:"egress_interface"`
	SourceIP        string   `yaml:"source_ip"`
}

type NAT struct {
	Sources []string `yaml:"sources"`
}

type RouterRoute struct {
	To        string `yaml:"to"`
	Via       string `yaml:"via"`
	Interface string `yaml:"interface"`
}

type Switch struct {
	Name          string `yaml:"name"`
	MTU           int    `yaml:"mtu"`
	STP           bool   `yaml:"stp"`
	VLANFiltering bool   `yaml:"vlan_filtering"`
}

type Link struct {
	Name      string      `yaml:"name"`
	MTU       int         `yaml:"mtu"`
	Endpoints []*Endpoint `yaml:"endpoints"`
}

type Endpoint struct {
	Node      string      `yaml:"node"`
	Interface string      `yaml:"interface"`
	Egress    *Impairment `yaml:"egress,omitempty"`
	Port      *Port       `yaml:"port,omitempty"`
}

type Impairment struct {
	LatencyMS   float64 `yaml:"latency_ms"`
	JitterMS    float64 `yaml:"jitter_ms"`
	LossPercent float64 `yaml:"loss_percent"`
	RateMbps    float64 `yaml:"rate_mbps"`
}

type Port struct {
	Mode         string `yaml:"mode"`
	VLAN         *int   `yaml:"vlan,omitempty"`
	AllowedVLANs []int  `yaml:"allowed_vlans,omitempty"`
	NativeVLAN   *int   `yaml:"native_vlan,omitempty"`
}
