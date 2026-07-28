package config

// Config is the normalized v1 topology document. Map declaration order is
// retained internally and used by Marshal so order-sensitive resources remain
// stable in normalized output.
type Config struct {
	Name           string             `yaml:"name"`
	Templates      map[string]string  `yaml:"templates"`
	ManagementHost ManagementHost     `yaml:"management_host"`
	VMs            map[string]*VM     `yaml:"vms"`
	Routers        map[string]*Router `yaml:"routers"`
	Switches       map[string]*Switch `yaml:"switches"`
	Links          map[string]*Link   `yaml:"links"`
	order          declarationOrder
}

type declarationOrder struct {
	Templates                []string
	ManagementHostInterfaces []string
	VMs                      []string
	VMDisks                  map[string][]string
	VMInterfaces             map[string][]string
	VMVLANs                  map[string]map[string][]int
	Routers                  []string
	RouterIfaces             map[string][]string
	RouterVLANs              map[string]map[string][]int
	Switches                 []string
	Links                    []string
}

type ManagementHost struct {
	Interfaces map[string]*ManagementHostInterface `yaml:"interfaces"`
}

type ManagementHostInterface struct {
	MTU  int                `yaml:"mtu"`
	IPv4 ManagementHostIPv4 `yaml:"ipv4"`
	MAC  string             `yaml:"-"`
}

type ManagementHostIPv4 struct {
	Addresses []string `yaml:"addresses"`
}

type VM struct {
	Template          string                  `yaml:"template"`
	CPU               *CPU                    `yaml:"cpu,omitempty"`
	MemoryMB          int                     `yaml:"memory_mb"`
	MemoryBalloon     *MemoryBalloon          `yaml:"memory_balloon,omitempty"`
	RootDisk          RootDisk                `yaml:"root_disk"`
	AdditionalDisks   map[string]*Disk        `yaml:"additional_disks"`
	GuestAgent        bool                    `yaml:"guest_agent"`
	Boot              Boot                    `yaml:"boot"`
	SSHAuthorizedKeys []string                `yaml:"ssh_authorized_keys"`
	Interfaces        map[string]*VMInterface `yaml:"interfaces"`
	VCPUs             int                     `yaml:"-"`
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
}

type Disk struct {
	SizeGiB uint64 `yaml:"size_gib"`
	Bus     string `yaml:"bus"`
}

type Boot struct {
	Order  []string `yaml:"order"`
	Menu   bool     `yaml:"menu"`
	Strict bool     `yaml:"strict"`
}

type VMInterface struct {
	Model  string          `yaml:"model"`
	Queues int             `yaml:"queues"`
	MAC    string          `yaml:"mac"`
	MTU    int             `yaml:"mtu"`
	IPv4   *VMIPv4         `yaml:"ipv4,omitempty"`
	VLANs  map[int]*VMVLAN `yaml:"vlans"`
}

type VMVLAN struct {
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
	Interfaces map[string]*RouterInterface `yaml:"interfaces"`
	Routes     []RouterRoute               `yaml:"routes"`
}

type RouterInterface struct {
	MAC      string              `yaml:"mac"`
	MTU      int                 `yaml:"mtu"`
	IPv4     *RouterIPv4         `yaml:"ipv4,omitempty"`
	VLANs    map[int]*RouterVLAN `yaml:"vlans"`
	External *External           `yaml:"external,omitempty"`
	NAT      *NAT                `yaml:"nat,omitempty"`
}

type RouterVLAN struct {
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
	MTU           int  `yaml:"mtu"`
	STP           bool `yaml:"stp"`
	VLANFiltering bool `yaml:"vlan_filtering"`
}

type Link struct {
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
