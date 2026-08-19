package qemu

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type CPU struct {
	Model                   string
	Sockets, Cores, Threads int
}
type Memory struct {
	MiB     uint64
	Balloon bool
}
type Firmware struct{ Code, Vars string }
type DiskSpec struct {
	Name, Path, Bus, BlockNode, Serial string
	Format, Cache                      string
	Boot                               bool
}
type NICSpec struct {
	Name, Model, MAC, TAP string
	Queues                int
	Vhost                 bool
	TAPFDs, VhostFDs      []int
	Boot                  bool
}
type BootSpec struct {
	Order        []string
	Menu, Strict bool
}

type NodeSpec struct {
	Topology, Name                                             string
	CPU                                                        CPU
	Memory                                                     Memory
	Firmware                                                   Firmware
	QMPPath, PIDFile, SerialSocket, GuestAgentSocket, SeedPath string
	Disks                                                      []DiskSpec
	NICs                                                       []NICSpec
	Boot                                                       BootSpec
}

type Builder struct{ Executable string }

func NewBuilder(executable string) *Builder {
	if executable == "" {
		executable = "qemu-system-x86_64"
	}
	return &Builder{Executable: executable}
}

// Build returns an exec-compatible argv including argv[0]. File descriptor
// numbers in NICSpec are child-side descriptors prepared by the launcher.
func (b *Builder) Build(s NodeSpec) ([]string, error) {
	if s.Topology == "" || s.Name == "" {
		return nil, errors.New("topology and node names are required")
	}
	if s.CPU.Model == "" {
		s.CPU.Model = "host"
	}
	if s.CPU.Sockets <= 0 || s.CPU.Cores <= 0 || s.CPU.Threads <= 0 {
		return nil, errors.New("CPU topology values must be positive")
	}
	vcpus64 := int64(s.CPU.Sockets) * int64(s.CPU.Cores) * int64(s.CPU.Threads)
	if vcpus64 <= 0 || vcpus64 > int64(^uint(0)>>1) {
		return nil, errors.New("CPU topology overflows")
	}
	if s.Memory.MiB < 128 {
		return nil, errors.New("memory must be at least 128 MiB")
	}
	for _, p := range []struct{ name, value string }{{"OVMF code", s.Firmware.Code}, {"OVMF vars", s.Firmware.Vars}, {"QMP", s.QMPPath}, {"PID file", s.PIDFile}, {"serial socket", s.SerialSocket}} {
		if p.value == "" {
			return nil, fmt.Errorf("%s path is required", p.name)
		}
	}
	bootCategory, err := bootCategories(s.Boot.Order)
	if err != nil {
		return nil, err
	}
	identity := "topolab:" + s.Topology + ":" + s.Name
	args := []string{b.Executable,
		"-name", "guest=" + identity + ",debug-threads=on",
		"-uuid", deterministicUUID(s.Topology + "\x00" + s.Name),
		"-enable-kvm", "-machine", "q35,accel=kvm",
		"-cpu", s.CPU.Model,
		"-smp", fmt.Sprintf("cpus=%d,sockets=%d,cores=%d,threads=%d", vcpus64, s.CPU.Sockets, s.CPU.Cores, s.CPU.Threads),
		"-m", strconv.FormatUint(s.Memory.MiB, 10),
		"-display", "none", "-monitor", "none",
		"-qmp", "unix:" + s.QMPPath + ",server=on,wait=off",
		"-pidfile", s.PIDFile,
		"-drive", "if=pflash,format=raw,readonly=on,file=" + escapeComma(s.Firmware.Code),
		"-drive", "if=pflash,format=raw,file=" + escapeComma(s.Firmware.Vars),
		"-chardev", "socket,id=serial0,path=" + escapeComma(s.SerialSocket) + ",reconnect-ms=1000",
		"-serial", "chardev:serial0",
	}
	if s.Boot.Menu {
		args = append(args, "-boot", "menu=on")
	} else {
		args = append(args, "-boot", "menu=off")
	}
	if s.Boot.Strict {
		args[len(args)-1] += ",strict=on"
	}
	slots := newPCIAllocator()
	if s.Memory.Balloon {
		slot, err := slots.Next()
		if err != nil {
			return nil, err
		}
		args = append(args, "-device", "virtio-balloon-pci,id=balloon0,addr="+slot)
	}
	if s.GuestAgentSocket != "" {
		slot, err := slots.Next()
		if err != nil {
			return nil, err
		}
		args = append(args,
			"-device", "virtio-serial-pci,id=virtserial0,addr="+slot,
			"-chardev", "socket,path="+escapeComma(s.GuestAgentSocket)+",server=on,wait=off,id=qga0",
			"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0,id=qga-port0")
	}

	diskBoot := bootCategory["disk"]
	for i, d := range s.Disks {
		if d.Path == "" {
			return nil, fmt.Errorf("disk %d has no path", i)
		}
		id := ID("disk", d.Name)
		if d.BlockNode == "" {
			d.BlockNode = "blk-" + id
		}
		if d.Serial == "" {
			d.Serial = DiskSerial(d.Name)
		}
		if len(d.Serial) > 20 {
			return nil, fmt.Errorf("disk %s serial exceeds QEMU's 20-character limit", d.Name)
		}
		fileNode := "file-" + id
		format := d.Format
		if format == "" {
			format = "qcow2"
		}
		if format != "qcow2" && format != "raw" {
			return nil, fmt.Errorf("disk %s has unsupported format %q", d.Name, format)
		}
		cache := d.Cache
		if cache == "" {
			cache = "none"
		}
		direct := "on"
		if cache == "writeback" {
			direct = "off"
		} else if cache != "none" {
			return nil, fmt.Errorf("disk %s has unsupported cache policy %q", d.Name, cache)
		}
		cacheOptions := ",cache.direct=" + direct + ",cache.no-flush=off"
		protocol := "driver=file,node-name=" + fileNode + ",filename=" + escapeComma(d.Path) + ",auto-read-only=on,discard=unmap" + cacheOptions
		if format == "raw" {
			// LVM paths are block devices, which require QEMU's host_device
			// protocol rather than the regular-file protocol.
			protocol = "driver=host_device,node-name=" + fileNode + ",filename=" + escapeComma(d.Path) + cacheOptions
		}
		args = append(args,
			"-blockdev", protocol,
			"-blockdev", "driver="+format+",node-name="+d.BlockNode+",file="+fileNode+cacheOptions)
		boot := ""
		if diskBoot > 0 {
			boot = fmt.Sprintf(",bootindex=%d", diskBoot+i)
		}
		slot, err := slots.Next()
		if err != nil {
			return nil, err
		}
		switch d.Bus {
		case "", "scsi":
			ioid, ctl := "io-"+id, "scsi-"+id
			args = append(args, "-object", "iothread,id="+ioid, "-device", fmt.Sprintf("virtio-scsi-pci,id=%s,iothread=%s,addr=%s", ctl, ioid, slot), "-device", fmt.Sprintf("scsi-hd,drive=%s,bus=%s.0,channel=0,scsi-id=0,lun=0,serial=%s,id=dev-%s%s", d.BlockNode, ctl, d.Serial, id, boot))
		case "virtio":
			args = append(args, "-device", fmt.Sprintf("virtio-blk-pci,drive=%s,serial=%s,id=dev-%s,addr=%s%s", d.BlockNode, d.Serial, id, slot, boot))
		case "sata":
			ctl := "sata-" + id
			args = append(args, "-device", fmt.Sprintf("ich9-ahci,id=%s,addr=%s", ctl, slot), "-device", fmt.Sprintf("ide-hd,drive=%s,bus=%s.0,serial=%s,id=dev-%s%s", d.BlockNode, ctl, d.Serial, id, boot))
		case "ide":
			ctl := "ide-" + id
			args = append(args, "-device", fmt.Sprintf("piix3-ide,id=%s,addr=%s", ctl, slot), "-device", fmt.Sprintf("ide-hd,drive=%s,bus=%s.0,serial=%s,id=dev-%s%s", d.BlockNode, ctl, d.Serial, id, boot))
		case "nvme":
			args = append(args, "-device", fmt.Sprintf("nvme,drive=%s,serial=%s,id=dev-%s,addr=%s%s", d.BlockNode, d.Serial, id, slot, boot))
		default:
			return nil, fmt.Errorf("unsupported disk bus %q", d.Bus)
		}
	}

	netBoot := bootCategory["network"]
	for i, n := range s.NICs {
		if n.Model == "" {
			n.Model = "virtio"
		}
		if n.Queues == 0 {
			n.Queues = 1
		}
		if n.Queues < 1 {
			return nil, fmt.Errorf("NIC %s has invalid queue count", n.Name)
		}
		if n.Model != "virtio" && n.Queues != 1 {
			return nil, fmt.Errorf("NIC %s: multiqueue requires virtio", n.Name)
		}
		if n.MAC == "" || n.TAP == "" {
			return nil, fmt.Errorf("NIC %s requires MAC and TAP", n.Name)
		}
		id := ID("net", n.Name)
		netdev := "tap,id=" + id
		if len(n.TAPFDs) > 0 {
			if len(n.TAPFDs) != n.Queues {
				return nil, fmt.Errorf("NIC %s: want %d TAP FDs, got %d", n.Name, n.Queues, len(n.TAPFDs))
			}
			netdev += ",fds=" + joinInts(n.TAPFDs, ":")
		} else {
			if n.Queues > 1 {
				return nil, fmt.Errorf("NIC %s: multiqueue requires TAP FDs", n.Name)
			}
			netdev += ",ifname=" + escapeComma(n.TAP) + ",script=no,downscript=no"
		}
		if n.Vhost {
			netdev += ",vhost=on"
			if len(n.VhostFDs) > 0 {
				if len(n.TAPFDs) == 0 {
					return nil, fmt.Errorf("NIC %s: vhost FDs require TAP FDs", n.Name)
				}
				if len(n.VhostFDs) != n.Queues {
					return nil, fmt.Errorf("NIC %s: want %d vhost FDs", n.Name, n.Queues)
				}
				netdev += ",vhostfds=" + joinInts(n.VhostFDs, ":")
			}
		} else {
			netdev += ",vhost=off"
		}
		args = append(args, "-netdev", netdev)
		model := map[string]string{"virtio": "virtio-net-pci", "e1000e": "e1000e", "e1000": "e1000", "igb": "igb", "rtl8139": "rtl8139", "vmxnet3": "vmxnet3"}[n.Model]
		if model == "" {
			return nil, fmt.Errorf("unsupported NIC model %q", n.Model)
		}
		slot, err := slots.Next()
		if err != nil {
			return nil, err
		}
		dev := fmt.Sprintf("%s,netdev=%s,mac=%s,id=dev-%s,addr=%s", model, id, n.MAC, id, slot)
		if n.Queues > 1 {
			dev += fmt.Sprintf(",mq=on,vectors=%d", 2*n.Queues+2)
		}
		if netBoot > 0 {
			dev += fmt.Sprintf(",bootindex=%d", netBoot+i)
		}
		args = append(args, "-device", dev)
	}
	if s.SeedPath != "" {
		args = append(args, "-drive", "if=none,format=raw,readonly=on,id=seed,file="+escapeComma(s.SeedPath), "-device", "ide-cd,drive=seed,id=seed-cd")
	}
	return args, nil
}

func bootCategories(order []string) (map[string]int, error) {
	if len(order) == 0 {
		order = []string{"disk"}
	}
	out := map[string]int{}
	base := 1
	for _, category := range order {
		if category != "disk" && category != "network" {
			return nil, fmt.Errorf("invalid boot category %q", category)
		}
		if out[category] != 0 {
			return nil, fmt.Errorf("duplicate boot category %q", category)
		}
		out[category] = base
		base += 1000
	}
	return out, nil
}

type pciAllocator struct{ next int }

func newPCIAllocator() *pciAllocator { return &pciAllocator{next: 5} }
func (p *pciAllocator) Next() (string, error) {
	// q35 reserves slot 0x1f for the southbridge/LPC function.
	if p.next >= 31 {
		return "", errors.New("QEMU PCI slot capacity exceeded")
	}
	v := fmt.Sprintf("0x%x", p.next)
	p.next++
	return v, nil
}

var invalidID = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func ID(prefix, logical string) string {
	slug := strings.Trim(invalidID.ReplaceAllString(logical, "-"), "-.")
	if slug == "" {
		slug = "resource"
	}
	// QEMU block node names are limited to 31 bytes. The longest generated
	// wrapper is "file-" plus this ID, so keep the shared ID at 26 bytes.
	const maxID = 26
	const hashLength = 8
	maxSlug := maxID - len(prefix) - 2 - hashLength
	if maxSlug < 1 {
		maxSlug = 1
	}
	if len(slug) > maxSlug {
		slug = slug[:maxSlug]
	}
	s := sha256.Sum256([]byte(prefix + "\x00" + logical))
	return prefix + "-" + slug + "-" + hex.EncodeToString(s[:4])
}

func DiskSerial(logical string) string {
	s := sha256.Sum256([]byte("disk-serial\x00" + logical))
	return "topolab-" + hex.EncodeToString(s[:6])
}
func deterministicUUID(value string) string {
	b := sha256.Sum256([]byte(value))
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func escapeComma(v string) string { return strings.ReplaceAll(v, ",", ",,") }
func joinInts(values []int, sep string) string {
	p := make([]string, len(values))
	for i, v := range values {
		p[i] = strconv.Itoa(v)
	}
	return strings.Join(p, sep)
}
