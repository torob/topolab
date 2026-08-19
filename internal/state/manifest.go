package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const ManifestVersion = 3

type Manifest struct {
	Version                  int                                 `json:"version"`
	Topology                 string                              `json:"topology"`
	ConfigurationHash        string                              `json:"configuration_hash,omitempty"`
	HostNetworkHash          string                              `json:"host_network_hash,omitempty"`
	DatapathVersion          int                                 `json:"datapath_version,omitempty"`
	FabricNamespace          string                              `json:"fabric_namespace,omitempty"`
	ManagementHostNamespace  string                              `json:"management_host_namespace,omitempty"`
	UpdatedAt                time.Time                           `json:"updated_at"`
	Nodes                    map[string]*Node                    `json:"nodes,omitempty"`
	Links                    map[string]*Link                    `json:"links,omitempty"`
	ManagementHostInterfaces map[string]*ManagementHostInterface `json:"management_host_interfaces,omitempty"`
	// LegacyHostInterfaces is read only so down can safely remove v1 root
	// datapaths. New manifests never populate it.
	LegacyHostInterfaces map[string]*ManagementHostInterface `json:"host_interfaces,omitempty"`
	OwnedResources       []OwnedResource                     `json:"owned_resources,omitempty"`
	// NetworkState is the network package's versioned, ownership-bearing state.
	// Keeping it opaque here avoids making the persistent state package depend
	// on a Linux implementation package.
	NetworkState json.RawMessage     `json:"network_state,omitempty"`
	Helpers      map[string]*Helper  `json:"helpers,omitempty"`
	Storages     map[string]*Storage `json:"storages,omitempty"`
}

// Storage records the identity of a declared backend and any TopoLab-owned
// thin pool. Supplied VGs/PVs are deliberately never ownership records.
type Storage struct {
	Name             string       `json:"name"`
	Type             string       `json:"type"`
	RootProvisioning string       `json:"root_provisioning,omitempty"`
	VolumeGroup      string       `json:"volume_group,omitempty"`
	VGUUID           string       `json:"vg_uuid,omitempty"`
	PoolName         string       `json:"pool_name,omitempty"`
	PoolUUID         string       `json:"pool_uuid,omitempty"`
	PoolRequested    uint64       `json:"pool_requested_bytes,omitempty"`
	PoolSizeBytes    uint64       `json:"pool_size_bytes,omitempty"`
	DataPercent      float64      `json:"data_percent,omitempty"`
	MetadataPercent  float64      `json:"metadata_percent,omitempty"`
	ZeroNewBlocks    bool         `json:"zero_new_blocks,omitempty"`
	WipeOnCreate     bool         `json:"wipe_on_create,omitempty"`
	Warnings         []string     `json:"warnings,omitempty"`
	Pending          string       `json:"pending,omitempty"`
	Bases            []LVResource `json:"bases,omitempty"`
}

type LVResource struct {
	Name    string   `json:"name"`
	UUID    string   `json:"uuid"`
	Tags    []string `json:"tags"`
	Pending string   `json:"pending,omitempty"`
}

// Helper records a namespace-scoped process and every file it owns. IdentityArg
// must occur in the live command line before the process may be signalled.
type Helper struct {
	Kind       string   `json:"kind"`
	Router     string   `json:"router,omitempty"`
	Interface  string   `json:"interface,omitempty"`
	Namespace  string   `json:"namespace,omitempty"`
	ConfigPath string   `json:"config_path,omitempty"`
	PIDPath    string   `json:"pid_path,omitempty"`
	LeasePath  string   `json:"lease_path,omitempty"`
	Process    *Process `json:"process,omitempty"`
}

type Node struct {
	Kind                string          `json:"kind"`
	StorageIdentityHash string          `json:"storage_identity_hash,omitempty"`
	RestartHash         string          `json:"restart_hash,omitempty"`
	BootstrapHash       string          `json:"bootstrap_hash,omitempty"`
	BootstrapSnapshot   json.RawMessage `json:"bootstrap_snapshot,omitempty"`
	TemplatePath        string          `json:"template_path,omitempty"`
	TemplateDigest      string          `json:"template_digest,omitempty"`
	RootDisk            *Disk           `json:"root_disk,omitempty"`
	AdditionalDisks     []Disk          `json:"additional_disks,omitempty"`
	SeedPath            string          `json:"seed_path,omitempty"`
	SeedDigest          string          `json:"seed_digest,omitempty"`
	SeedPending         bool            `json:"seed_pending,omitempty"`
	OVMFVarsPath        string          `json:"ovmf_vars_path,omitempty"`
	PID                 int             `json:"pid,omitempty"`
	NetworkNamespace    string          `json:"network_namespace,omitempty"`
	PIDFile             string          `json:"pid_file,omitempty"`
	QMPPath             string          `json:"qmp_path,omitempty"`
	SerialSocketPath    string          `json:"serial_socket_path,omitempty"`
	SerialLogPath       string          `json:"serial_log_path,omitempty"`
	GuestAgentPath      string          `json:"guest_agent_path,omitempty"`
	BalloonTargetMB     *uint64         `json:"balloon_target_mb,omitempty"`
	TAPs                map[string]TAP  `json:"taps,omitempty"`
	Hardware            json.RawMessage `json:"hardware,omitempty"`
	Process             *Process        `json:"process,omitempty"`
	CollectorProcess    *Process        `json:"collector_process,omitempty"`
}

type Disk struct {
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	Bus           string   `json:"bus"`
	BlockNode     string   `json:"block_node"`
	Serial        string   `json:"serial"`
	CapacityBytes uint64   `json:"capacity_bytes"`
	Storage       string   `json:"storage"`
	StorageType   string   `json:"storage_type"`
	Provisioning  string   `json:"provisioning,omitempty"`
	Format        string   `json:"format"`
	VolumeGroup   string   `json:"volume_group,omitempty"`
	VGUUID        string   `json:"vg_uuid,omitempty"`
	LVName        string   `json:"lv_name,omitempty"`
	LVUUID        string   `json:"lv_uuid,omitempty"`
	PoolName      string   `json:"pool_name,omitempty"`
	PoolUUID      string   `json:"pool_uuid,omitempty"`
	OriginName    string   `json:"origin_name,omitempty"`
	OriginUUID    string   `json:"origin_uuid,omitempty"`
	OwnershipTags []string `json:"ownership_tags,omitempty"`
	Pending       string   `json:"pending,omitempty"`
	StagingName   string   `json:"staging_name,omitempty"`
	StagingUUID   string   `json:"staging_uuid,omitempty"`
	WipeOnCreate  bool     `json:"wipe_on_create,omitempty"`
}

type TAP struct {
	Name       string `json:"name"`
	MAC        string `json:"mac,omitempty"`
	Queues     int    `json:"queues,omitempty"`
	Datapath   string `json:"datapath,omitempty"`
	OwnerAlias string `json:"owner_alias,omitempty"`
}

type Process struct {
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	Executable  string    `json:"executable"`
	IdentityArg string    `json:"identity_arg"`
}

type Link struct {
	GenerationID        string         `json:"generation_id"`
	GenerationStartedAt time.Time      `json:"generation_started_at"`
	Endpoints           []LinkEndpoint `json:"endpoints"`
	OwnedDevices        []string       `json:"owned_devices,omitempty"`
}

type LinkEndpoint struct {
	Node             string `json:"node"`
	Interface        string `json:"interface"`
	IngressDevice    string `json:"ingress_device"`
	EgressDevice     string `json:"egress_device"`
	ImpairmentDevice string `json:"impairment_device,omitempty"`
}

type ManagementHostInterface struct {
	LogicalName     string   `json:"logical_name"`
	DeviceName      string   `json:"device_name"`
	OwnerAlias      string   `json:"owner_alias"`
	MTU             int      `json:"mtu"`
	Addresses       []string `json:"addresses"`
	Link            string   `json:"link"`
	IsolationRuleID string   `json:"isolation_rule_id"`
}

type OwnedResource struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Marker string `json:"marker,omitempty"`
	Path   string `json:"path,omitempty"`
}

func NewManifest(topology string) *Manifest {
	return &Manifest{Version: ManifestVersion, Topology: topology, Nodes: make(map[string]*Node), Links: make(map[string]*Link), ManagementHostInterfaces: make(map[string]*ManagementHostInterface), Helpers: make(map[string]*Helper), Storages: make(map[string]*Storage)}
}

func Load(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 16<<20))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	return &m, nil
}

func Save(path string, m *Manifest) error {
	if m == nil {
		return errors.New("nil manifest")
	}
	m.Version = ManifestVersion
	m.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	data = append(data, '\n')
	return AtomicWrite(path, data, 0o600)
}

func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".topolab-atomic-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func Hash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func ShortID(value string, n int) string {
	s := sha256.Sum256([]byte(value))
	encoded := hex.EncodeToString(s[:])
	if n <= 0 || n > len(encoded) {
		return encoded
	}
	return encoded[:n]
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("manifest contains multiple JSON values")
		}
		return fmt.Errorf("decode manifest trailing data: %w", err)
	}
	return nil
}
