package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

var (
	resourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	routerIface  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$`)
	leasePattern = regexp.MustCompile(`^[1-9][0-9]*[smhdw]$`)
	lvmName      = regexp.MustCompile(`^[A-Za-z0-9_+][A-Za-z0-9_.+-]{0,126}$`)
)

func validateDecoded(c *Config) error {
	if c.Remote != nil {
		if err := ValidateDestination(c.Remote.SSH); err != nil {
			return fmt.Errorf("remote.ssh: %w", err)
		}
	}
	for _, storage := range c.Storages {
		if storage == nil {
			continue
		}
		if err := validateStorageFields(storage); err != nil {
			return err
		}
	}
	for _, vm := range c.VMs {
		name := vm.Name
		if vm == nil {
			continue
		}
		if vm.CPU != nil {
			set := 0
			if vm.CPU.Sockets != nil {
				set++
			}
			if vm.CPU.Cores != nil {
				set++
			}
			if vm.CPU.Threads != nil {
				set++
			}
			if set != 0 && set != 3 {
				return fmt.Errorf("VM %q must specify all of cpu.sockets, cpu.cores, and cpu.threads", name)
			}
		}
	}
	return nil
}

func validateStorageFields(s *Storage) error {
	prefix := fmt.Sprintf("storage %q", s.Name)
	required := func(field, value string, _ bool) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s field %q is required for type %q", prefix, field, s.Type)
		}
		return nil
	}
	forbidden := func(field string, set bool) error {
		if set {
			return fmt.Errorf("%s field %q is forbidden for type %q", prefix, field, s.Type)
		}
		return nil
	}
	switch s.Type {
	case "file":
		if err := required("root_provisioning", s.RootProvisioning, s.rootProvisioningSet); err != nil {
			return err
		}
		if err := forbidden("volume_group", s.volumeGroupSet); err != nil {
			return err
		}
		if err := forbidden("pool_size_gib", s.poolSizeGiBSet); err != nil {
			return err
		}
		if err := forbidden("zero_new_blocks", s.zeroNewBlocksSet); err != nil {
			return err
		}
		if err := forbidden("wipe_on_create", s.wipeOnCreateSet); err != nil {
			return err
		}
	case "lvm-thin":
		if err := required("volume_group", s.VolumeGroup, s.volumeGroupSet); err != nil {
			return err
		}
		if !s.poolSizeGiBSet || s.PoolSizeGiB == 0 {
			return fmt.Errorf("%s field %q must be positive for type %q", prefix, "pool_size_gib", s.Type)
		}
		if err := required("root_provisioning", s.RootProvisioning, s.rootProvisioningSet); err != nil {
			return err
		}
		if err := forbidden("wipe_on_create", s.wipeOnCreateSet); err != nil {
			return err
		}
	case "lvm-thick":
		if err := required("volume_group", s.VolumeGroup, s.volumeGroupSet); err != nil {
			return err
		}
		if err := forbidden("pool_size_gib", s.poolSizeGiBSet); err != nil {
			return err
		}
		if err := forbidden("root_provisioning", s.rootProvisioningSet); err != nil {
			return err
		}
		if err := forbidden("zero_new_blocks", s.zeroNewBlocksSet); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s has unsupported type %q (want file, lvm-thin, or lvm-thick)", prefix, s.Type)
	}
	if s.rootProvisioningSet && s.RootProvisioning != "copy" && s.RootProvisioning != "snapshot" {
		return fmt.Errorf("%s field %q must be copy or snapshot for type %q", prefix, "root_provisioning", s.Type)
	}
	if (s.Type == "lvm-thin" || s.Type == "lvm-thick") && !lvmName.MatchString(s.VolumeGroup) {
		return fmt.Errorf("%s field %q has invalid LVM name %q for type %q", prefix, "volume_group", s.VolumeGroup, s.Type)
	}
	return nil
}

func validate(c *Config) ([]string, error) {
	if !resourceName.MatchString(c.Name) {
		return nil, fmt.Errorf("name %q must match %s", c.Name, resourceName)
	}
	for _, template := range c.Templates {
		if err := named("template", template.Name); err != nil {
			return nil, err
		}
	}
	for _, storage := range c.Storages {
		if err := named("storage", storage.Name); err != nil {
			return nil, err
		}
	}
	nodes := map[string]string{"management_host": "reserved management host"}
	for kind, names := range map[string][]string{"VM": c.VMNames(), "router": c.RouterNames(), "switch": c.SwitchNames()} {
		for _, name := range names {
			if err := named(kind, name); err != nil {
				return nil, err
			}
			if prior, ok := nodes[name]; ok {
				return nil, fmt.Errorf("%s %q conflicts with %s in the global node namespace", kind, name, prior)
			}
			nodes[name] = kind
		}
	}
	for _, link := range c.Links {
		if err := named("link", link.Name); err != nil {
			return nil, err
		}
	}
	seenMAC, seenIP := map[string]string{}, map[netip.Addr]string{}
	for _, h := range c.ManagementHost.Interfaces {
		name := h.Name
		if err := named("management-host interface", name); err != nil {
			return nil, err
		}
		if err := validMTU(h.MTU, "management-host interface "+name); err != nil {
			return nil, err
		}
		if len(h.IPv4.Addresses) == 0 {
			return nil, fmt.Errorf("management-host interface %q requires at least one IPv4 address", name)
		}
		if err := validateAddresses(h.IPv4.Addresses, "management_host."+name, seenIP); err != nil {
			return nil, err
		}
		if err := recordMAC(h.MAC, "management_host."+name, seenMAC); err != nil {
			return nil, err
		}
	}
	for _, vm := range c.VMs {
		if err := validateVM(c, vm.Name, vm, seenMAC, seenIP); err != nil {
			return nil, err
		}
	}
	externalCount := 0
	for _, r := range c.Routers {
		n, err := validateRouter(r.Name, r, seenMAC, seenIP)
		if err != nil {
			return nil, err
		}
		externalCount += n
	}
	if len(c.Links) == 0 && externalCount == 0 {
		return nil, errors.New("topology requires at least one link or external router interface")
	}
	for _, sw := range c.Switches {
		if err := validMTU(sw.MTU, "switch "+sw.Name); err != nil {
			return nil, err
		}
	}
	warnings, err := validateLinks(c)
	if err != nil {
		return nil, err
	}
	return warnings, nil
}

func validateVM(c *Config, name string, vm *VM, macs map[string]string, ips map[netip.Addr]string) error {
	if _, ok := c.LookupTemplate(vm.Template); !ok {
		return fmt.Errorf("VM %q references unknown template %q", name, vm.Template)
	}
	if vm.MemoryMB < 128 {
		return fmt.Errorf("VM %q memory_mb must be at least 128", name)
	}
	if vm.CPU.Model == "" || strings.Contains(vm.CPU.Model, ",") {
		return fmt.Errorf("VM %q has invalid CPU model %q", name, vm.CPU.Model)
	}
	vals := []*int{vm.CPU.Sockets, vm.CPU.Cores, vm.CPU.Threads}
	product := uint64(1)
	for _, v := range vals {
		if v == nil || *v <= 0 {
			return fmt.Errorf("VM %q CPU topology values must be positive", name)
		}
		if product > math.MaxInt/uint64(*v) {
			return fmt.Errorf("VM %q CPU topology overflows", name)
		}
		product *= uint64(*v)
	}
	vm.VCPUs = int(product)
	if vm.MemoryBalloon.Enabled {
		if vm.MemoryBalloon.TargetMB == nil || *vm.MemoryBalloon.TargetMB < 128 || *vm.MemoryBalloon.TargetMB > vm.MemoryMB {
			return fmt.Errorf("VM %q balloon target_mb must be between 128 and memory_mb", name)
		}
	} else if vm.MemoryBalloon.TargetMB != nil {
		return fmt.Errorf("VM %q memory_balloon.target_mb requires enabled: true", name)
	}
	if err := validBus(vm.RootDisk.Bus); err != nil {
		return fmt.Errorf("VM %q root_disk: %w", name, err)
	}
	if err := validDiskCache(vm.RootDisk.Cache); err != nil {
		return fmt.Errorf("VM %q root_disk: %w", name, err)
	}
	if err := validDiskAIO(vm.RootDisk.AIO, vm.RootDisk.Cache); err != nil {
		return fmt.Errorf("VM %q root_disk: %w", name, err)
	}
	if vm.RootDisk.Storage == "" {
		return fmt.Errorf("VM %q root_disk.storage is required", name)
	}
	if _, ok := c.LookupStorage(vm.RootDisk.Storage); !ok {
		return fmt.Errorf("VM %q root_disk references unknown storage %q", name, vm.RootDisk.Storage)
	}
	if vm.RootDisk.SizeGiB != nil && *vm.RootDisk.SizeGiB == 0 {
		return fmt.Errorf("VM %q root disk size_gib must be positive", name)
	}
	for _, d := range vm.AdditionalDisks {
		diskName := d.Name
		if err := named("disk", diskName); err != nil {
			return err
		}
		if d.SizeGiB == 0 {
			return fmt.Errorf("VM %q disk %q size_gib must be positive", name, diskName)
		}
		if d.Storage == "" {
			return fmt.Errorf("VM %q disk %q storage is required", name, diskName)
		}
		if _, ok := c.LookupStorage(d.Storage); !ok {
			return fmt.Errorf("VM %q disk %q references unknown storage %q", name, diskName, d.Storage)
		}
		if err := validBus(d.Bus); err != nil {
			return fmt.Errorf("VM %q disk %q: %w", name, diskName, err)
		}
		if err := validDiskCache(d.Cache); err != nil {
			return fmt.Errorf("VM %q disk %q: %w", name, diskName, err)
		}
		if err := validDiskAIO(d.AIO, d.Cache); err != nil {
			return fmt.Errorf("VM %q disk %q: %w", name, diskName, err)
		}
	}
	if len(vm.Boot.Order) == 0 {
		return fmt.Errorf("VM %q boot.order must not be empty", name)
	}
	seenBoot := map[string]bool{}
	for _, b := range vm.Boot.Order {
		if b != "disk" && b != "network" {
			return fmt.Errorf("VM %q boot.order contains %q", name, b)
		}
		if seenBoot[b] {
			return fmt.Errorf("VM %q boot.order duplicates %q", name, b)
		}
		seenBoot[b] = true
	}
	if len(vm.SSHAuthorizedKeys) == 0 {
		return fmt.Errorf("VM %q requires ssh_authorized_keys", name)
	}
	if err := validateKeys(vm.SSHAuthorizedKeys, "VM "+name); err != nil {
		return err
	}
	if len(vm.Interfaces) == 0 {
		return fmt.Errorf("VM %q requires at least one interface", name)
	}
	for _, iface := range vm.Interfaces {
		ifaceName := iface.Name
		if err := named("VM interface", ifaceName); err != nil {
			return err
		}
		if len(ifaceName) > 15 {
			return fmt.Errorf("VM %q interface %q exceeds 15 characters", name, ifaceName)
		}
		if !oneOf(iface.Model, "virtio", "e1000e", "e1000", "igb", "rtl8139", "vmxnet3") {
			return fmt.Errorf("VM %q interface %q has unsupported model %q", name, ifaceName, iface.Model)
		}
		if iface.Queues <= 0 || iface.Queues > vm.VCPUs {
			return fmt.Errorf("VM %q interface %q queues must be from 1 through %d", name, ifaceName, vm.VCPUs)
		}
		if iface.Model != "virtio" && iface.Queues != 1 {
			return fmt.Errorf("VM %q interface %q multiqueue requires model virtio", name, ifaceName)
		}
		if err := validMTU(iface.MTU, "VM "+name+" interface "+ifaceName); err != nil {
			return err
		}
		if err := recordMAC(iface.MAC, name+"."+ifaceName, macs); err != nil {
			return err
		}
		if err := validateVMIPv4(iface.IPv4, name+"."+ifaceName, ips); err != nil {
			return err
		}
		for _, vlan := range iface.VLANs {
			id := vlan.ID
			if id < 1 || id > 4094 {
				return fmt.Errorf("VM %q interface %q VLAN must be from 1 through 4094", name, ifaceName)
			}
			if len(fmt.Sprintf("%s.%d", ifaceName, id)) > 15 {
				return fmt.Errorf("VM %q VLAN interface %s.%d exceeds 15 characters", name, ifaceName, id)
			}
			if err := validMTU(vlan.MTU, "VM VLAN"); err != nil {
				return err
			}
			if vlan.MTU > iface.MTU {
				return fmt.Errorf("VM %q VLAN %s.%d MTU exceeds base interface", name, ifaceName, id)
			}
			if err := validateVMIPv4(vlan.IPv4, fmt.Sprintf("%s.%s.%d", name, ifaceName, id), ips); err != nil {
				return err
			}
		}
	}
	return nil
}

func validDiskCache(cache string) error {
	if cache != "none" && cache != "writeback" {
		return fmt.Errorf("cache must be none or writeback, got %q", cache)
	}
	return nil
}

func validDiskAIO(aio, cache string) error {
	if aio != "auto" && aio != "io_uring" && aio != "threads" && aio != "native" {
		return fmt.Errorf("aio must be auto, io_uring, threads, or native, got %q", aio)
	}
	if aio == "native" && cache != "none" {
		return errors.New("aio native requires cache none")
	}
	return nil
}

func validateVMIPv4(ip *VMIPv4, where string, global map[netip.Addr]string) error {
	if ip == nil {
		return nil
	}
	if ip.DHCP && len(ip.Addresses) != 0 {
		return fmt.Errorf("%s may not combine DHCP and static addresses", where)
	}
	if ip.DHCP && len(ip.DNSServers) != 0 {
		return fmt.Errorf("%s may not set dns_servers with DHCP", where)
	}
	prefixes, err := parseAddresses(ip.Addresses, where, global)
	if err != nil {
		return err
	}
	seenDNS := map[netip.Addr]bool{}
	for _, s := range ip.DNSServers {
		a, err := parseAddr(s)
		if err != nil {
			return fmt.Errorf("%s DNS server: %w", where, err)
		}
		if seenDNS[a] {
			return fmt.Errorf("%s duplicates DNS server %s", where, a)
		}
		seenDNS[a] = true
	}
	seenRoutes := map[string]bool{}
	for _, r := range ip.Routes {
		to, err := parseDestination(r.To)
		if err != nil {
			return fmt.Errorf("%s route: %w", where, err)
		}
		if seenRoutes[to] {
			return fmt.Errorf("%s duplicates route destination %s", where, to)
		}
		seenRoutes[to] = true
		via, err := parseAddr(r.Via)
		if err != nil {
			return fmt.Errorf("%s route next hop: %w", where, err)
		}
		if !covered(via, prefixes) {
			return fmt.Errorf("%s route next hop %s is not reachable through that layer", where, via)
		}
	}
	return nil
}

func validateRouter(name string, r *Router, macs map[string]string, ips map[netip.Addr]string) (int, error) {
	if len(r.Interfaces) == 0 {
		return 0, fmt.Errorf("router %q requires at least one interface", name)
	}
	externals := 0
	layers := map[string][]netip.Prefix{}
	for _, iface := range r.Interfaces {
		ifaceName := iface.Name
		if !routerIface.MatchString(ifaceName) {
			return 0, fmt.Errorf("router %q interface %q has an invalid name", name, ifaceName)
		}
		if err := validMTU(iface.MTU, "router "+name+" interface "+ifaceName); err != nil {
			return 0, err
		}
		if err := recordMAC(iface.MAC, name+"."+ifaceName, macs); err != nil {
			return 0, err
		}
		if iface.External != nil {
			externals++
			if iface.IPv4 != nil || len(iface.VLANs) != 0 || iface.NAT != nil {
				return 0, fmt.Errorf("router %q external interface %q may not set ipv4, vlans, or nat", name, ifaceName)
			}
			if len(iface.External.Sources) == 0 {
				return 0, fmt.Errorf("router %q external interface %q requires sources", name, ifaceName)
			}
			if err := validatePrefixes(iface.External.Sources, "external sources"); err != nil {
				return 0, err
			}
			if iface.External.EgressInterface == "" {
				return 0, fmt.Errorf("router %q external egress_interface is empty", name)
			}
			if iface.External.SourceIP != "auto" {
				if _, err := parseAddr(iface.External.SourceIP); err != nil {
					return 0, fmt.Errorf("router %q external source_ip: %w", name, err)
				}
			}
			continue
		}
		p, err := validateRouterIPv4(iface.IPv4, name+"."+ifaceName, ips)
		if err != nil {
			return 0, err
		}
		layers[ifaceName] = p
		if iface.NAT != nil {
			if len(iface.NAT.Sources) == 0 {
				return 0, fmt.Errorf("router %q interface %q nat requires sources", name, ifaceName)
			}
			if err := validatePrefixes(iface.NAT.Sources, "nat sources"); err != nil {
				return 0, err
			}
		}
		for _, vlan := range iface.VLANs {
			id := vlan.ID
			if id < 1 || id > 4094 || len(fmt.Sprintf("%s.%d", ifaceName, id)) > 15 {
				return 0, fmt.Errorf("router %q has invalid VLAN interface %s.%d", name, ifaceName, id)
			}
			if err := validMTU(vlan.MTU, "router VLAN"); err != nil {
				return 0, err
			}
			if vlan.MTU > iface.MTU {
				return 0, fmt.Errorf("router %q VLAN %s.%d MTU exceeds base", name, ifaceName, id)
			}
			p, err := validateRouterIPv4(vlan.IPv4, fmt.Sprintf("%s.%s.%d", name, ifaceName, id), ips)
			if err != nil {
				return 0, err
			}
			layers[fmt.Sprintf("%s.%d", ifaceName, id)] = p
		}
	}
	if externals > 1 {
		return 0, fmt.Errorf("router %q may have at most one external interface", name)
	}
	seenRoutes := map[string]bool{}
	for _, route := range r.Routes {
		to, err := parseDestination(route.To)
		if err != nil {
			return 0, fmt.Errorf("router %q route: %w", name, err)
		}
		if seenRoutes[to] {
			return 0, fmt.Errorf("router %q duplicates route destination %s", name, to)
		}
		seenRoutes[to] = true
		if externals != 0 && to == "default" {
			return 0, fmt.Errorf("router %q with an external interface may not declare a default route", name)
		}
		prefixes, ok := layers[route.Interface]
		if !ok {
			return 0, fmt.Errorf("router %q route names unknown or external interface %q", name, route.Interface)
		}
		via, err := parseAddr(route.Via)
		if err != nil || !covered(via, prefixes) {
			return 0, fmt.Errorf("router %q route next hop %q is not reachable through %s", name, route.Via, route.Interface)
		}
		if to != "default" {
			dest, _ := netip.ParsePrefix(to)
			for _, p := range prefixes {
				if dest.Masked() == p.Masked() {
					return 0, fmt.Errorf("router %q route %s duplicates a connected route", name, to)
				}
			}
		}
	}
	return externals, nil
}

func validateRouterIPv4(ip *RouterIPv4, where string, global map[netip.Addr]string) ([]netip.Prefix, error) {
	if ip == nil {
		return nil, nil
	}
	prefixes, err := parseAddresses(ip.Addresses, where, global)
	if err != nil {
		return nil, err
	}
	if d := ip.DHCPServer; d != nil {
		if len(prefixes) == 0 {
			return nil, fmt.Errorf("%s DHCP server requires static addresses", where)
		}
		start, e1 := parseAddr(d.RangeStart)
		end, e2 := parseAddr(d.RangeEnd)
		if e1 != nil || e2 != nil || start.Compare(end) > 0 || !covered(start, prefixes) || !covered(end, prefixes) {
			return nil, fmt.Errorf("%s has an invalid DHCP range", where)
		}
		samePrefix := false
		for _, prefix := range prefixes {
			if prefix.Contains(start) && prefix.Contains(end) {
				samePrefix = true
				break
			}
		}
		if !samePrefix {
			return nil, fmt.Errorf("%s DHCP range endpoints must belong to one connected prefix", where)
		}
		if d.Lease != "infinite" && !leasePattern.MatchString(d.Lease) {
			return nil, fmt.Errorf("%s has invalid DHCP lease %q", where, d.Lease)
		}
		seen := map[netip.Addr]bool{}
		for _, s := range d.DNSServers {
			a, err := parseAddr(s)
			if err != nil {
				return nil, err
			}
			if seen[a] {
				return nil, fmt.Errorf("%s duplicates DHCP DNS server %s", where, a)
			}
			seen[a] = true
		}
	}
	return prefixes, nil
}

func validateLinks(c *Config) ([]string, error) {
	uses := map[string]int{}
	ports := map[string]bool{}
	var warnings []string
	for _, link := range c.Links {
		linkName := link.Name
		if err := validMTU(link.MTU, "link "+linkName); err != nil {
			return nil, err
		}
		if len(link.Endpoints) != 2 {
			return nil, fmt.Errorf("link %q must have exactly two endpoints", linkName)
		}
		if link.Endpoints[0] == nil || link.Endpoints[1] == nil {
			return nil, fmt.Errorf("link %q has a null endpoint", linkName)
		}
		if link.Endpoints[0].Node == link.Endpoints[1].Node {
			return nil, fmt.Errorf("link %q may not link node %q to itself", linkName, link.Endpoints[0].Node)
		}
		for i, ep := range link.Endpoints {
			where := ep.Node + ":" + ep.Interface
			if ep.Egress != nil && (nonFinite(ep.Egress.LatencyMS) || nonFinite(ep.Egress.JitterMS) || nonFinite(ep.Egress.LossPercent) || nonFinite(ep.Egress.RateMbps) || ep.Egress.LatencyMS < 0 || ep.Egress.JitterMS < 0 || ep.Egress.LossPercent < 0 || ep.Egress.LossPercent > 100 || ep.Egress.RateMbps < 0) {
				return nil, fmt.Errorf("link %q endpoint %s has invalid egress impairment", linkName, where)
			}
			nodeMTU := 0
			switch ep.Node {
			case "management_host":
				h, ok := c.LookupManagementHostInterface(ep.Interface)
				if !ok {
					return nil, fmt.Errorf("link %q references unknown management-host interface %q", linkName, ep.Interface)
				}
				if ep.Port != nil {
					return nil, fmt.Errorf("management-host endpoint %s may not set port", where)
				}
				uses[where]++
				nodeMTU = h.MTU
			default:
				if vm, ok := c.LookupVM(ep.Node); ok {
					iface, ok := c.LookupVMInterface(vm.Name, ep.Interface)
					if !ok {
						return nil, fmt.Errorf("link %q references unknown VM interface %s", linkName, where)
					}
					if ep.Port != nil {
						return nil, fmt.Errorf("VM endpoint %s may not set port", where)
					}
					uses[where]++
					nodeMTU = iface.MTU
				} else if r, ok := c.LookupRouter(ep.Node); ok {
					iface, ok := c.LookupRouterInterface(r.Name, ep.Interface)
					if !ok {
						return nil, fmt.Errorf("link %q references unknown router interface %s", linkName, where)
					}
					if iface.External != nil {
						return nil, fmt.Errorf("external router interface %s may not occur in a link", where)
					}
					if ep.Port != nil {
						return nil, fmt.Errorf("router endpoint %s may not set port", where)
					}
					uses[where]++
					nodeMTU = iface.MTU
				} else if sw, ok := c.LookupSwitch(ep.Node); ok {
					if err := named("switch port", ep.Interface); err != nil {
						return nil, err
					}
					if ports[where] {
						return nil, fmt.Errorf("switch port %s is used more than once", where)
					}
					ports[where] = true
					if err := validatePort(ep.Port, sw); err != nil {
						return nil, fmt.Errorf("switch endpoint %s: %w", where, err)
					}
					nodeMTU = sw.MTU
				} else {
					return nil, fmt.Errorf("link %q references unknown node %q", linkName, ep.Node)
				}
			}
			if nodeMTU != link.MTU {
				warnings = append(warnings, fmt.Sprintf("link %s MTU %d differs from endpoint %s MTU %d", linkName, link.MTU, where, nodeMTU))
			}
			_ = i
		}
		for _, ep := range link.Endpoints {
			if ep.Node == "management_host" {
				peer := link.Endpoints[0]
				if peer == ep {
					peer = link.Endpoints[1]
				}
				if _, ok := c.LookupSwitch(peer.Node); ok && peer.Port != nil && peer.Port.Mode == "trunk" {
					return nil, fmt.Errorf("management-host endpoint on link %q may not connect to a switch trunk", linkName)
				}
			}
		}
	}
	for _, iface := range c.ManagementHost.Interfaces {
		name := iface.Name
		if uses["management_host:"+name] != 1 {
			return nil, fmt.Errorf("management-host interface %q must occur in exactly one link", name)
		}
	}
	for _, vm := range c.VMs {
		node := vm.Name
		for _, iface := range vm.Interfaces {
			name := iface.Name
			if uses[node+":"+name] != 1 {
				return nil, fmt.Errorf("VM interface %s:%s must occur in exactly one link", node, name)
			}
		}
	}
	for _, r := range c.Routers {
		node := r.Name
		for _, iface := range r.Interfaces {
			name := iface.Name
			want := 1
			if iface.External != nil {
				want = 0
			}
			if uses[node+":"+name] != want {
				return nil, fmt.Errorf("router interface %s:%s occurs %d times; want %d", node, name, uses[node+":"+name], want)
			}
		}
	}
	return warnings, nil
}

func nonFinite(value float64) bool { return math.IsNaN(value) || math.IsInf(value, 0) }

func validatePort(p *Port, sw *Switch) error {
	if p == nil {
		return errors.New("internal: switch port was not defaulted")
	}
	if p.Mode == "access" {
		if p.VLAN == nil || *p.VLAN < 1 || *p.VLAN > 4094 {
			return errors.New("access VLAN must be from 1 through 4094")
		}
		if len(p.AllowedVLANs) != 0 || p.NativeVLAN != nil {
			return errors.New("access port may not set trunk fields")
		}
		if *p.VLAN != 1 && !sw.VLANFiltering {
			return errors.New("access VLAN other than 1 requires vlan_filtering")
		}
		return nil
	}
	if p.Mode != "trunk" {
		return fmt.Errorf("mode must be access or trunk, got %q", p.Mode)
	}
	if p.VLAN != nil {
		return errors.New("trunk port may not set vlan")
	}
	if !sw.VLANFiltering {
		return errors.New("trunk requires vlan_filtering")
	}
	if len(p.AllowedVLANs) == 0 {
		return errors.New("trunk requires allowed_vlans")
	}
	seen := map[int]bool{}
	for _, v := range p.AllowedVLANs {
		if v < 1 || v > 4094 || seen[v] {
			return errors.New("trunk allowed_vlans must be unique IDs from 1 through 4094")
		}
		seen[v] = true
	}
	if p.NativeVLAN != nil && !seen[*p.NativeVLAN] {
		return errors.New("native_vlan must be allowed")
	}
	return nil
}

func named(kind, name string) error {
	if !resourceName.MatchString(name) {
		return fmt.Errorf("%s name %q must match %s", kind, name, resourceName)
	}
	return nil
}
func validMTU(v int, where string) error {
	if v < 576 || v > 9216 {
		return fmt.Errorf("%s MTU must be from 576 through 9216", where)
	}
	return nil
}
func validBus(v string) error {
	if !oneOf(v, "scsi", "virtio", "sata", "nvme", "ide") {
		return fmt.Errorf("unsupported disk bus %q", v)
	}
	return nil
}
func oneOf(v string, values ...string) bool {
	for _, x := range values {
		if v == x {
			return true
		}
	}
	return false
}
func validateKeys(keys []string, where string) error {
	seen := map[string]bool{}
	for _, key := range keys {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
		if err != nil {
			return fmt.Errorf("%s has invalid authorized key: %w", where, err)
		}
		canonical := string(ssh.MarshalAuthorizedKey(pub))
		if seen[canonical] {
			return fmt.Errorf("%s duplicates an authorized key", where)
		}
		seen[canonical] = true
	}
	return nil
}
func recordMAC(value, where string, seen map[string]string) error {
	m, err := net.ParseMAC(value)
	if err != nil || len(m) != 6 || m[0]&1 != 0 {
		return fmt.Errorf("%s has invalid unicast MAC %q", where, value)
	}
	key := m.String()
	if prior, ok := seen[key]; ok {
		return fmt.Errorf("MAC %s is duplicated by %s and %s", key, prior, where)
	}
	seen[key] = where
	return nil
}

func parseAddr(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || !a.IsValid() || a.IsUnspecified() || a.IsMulticast() || a == netip.IPv4Unspecified() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.Addr{}, fmt.Errorf("%q is not a usable IPv4 address", s)
	}
	return a, nil
}
func parseAddresses(values []string, where string, global map[netip.Addr]string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(values))
	local := map[netip.Addr]bool{}
	for _, s := range values {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("%s has invalid IPv4 prefix %q", where, s)
		}
		a, err := parseAddr(p.Addr().String())
		if err != nil || !usableInPrefix(a, p) {
			return nil, fmt.Errorf("%s address %q is not usable", where, s)
		}
		if local[a] {
			return nil, fmt.Errorf("%s duplicates address %s", where, a)
		}
		if prior, ok := global[a]; ok {
			return nil, fmt.Errorf("IPv4 address %s is duplicated by %s and %s", a, prior, where)
		}
		local[a] = true
		global[a] = where
		out = append(out, p)
	}
	return out, nil
}
func validateAddresses(values []string, where string, global map[netip.Addr]string) error {
	_, err := parseAddresses(values, where, global)
	return err
}
func usableInPrefix(a netip.Addr, p netip.Prefix) bool {
	if p.Bits() >= 31 {
		return true
	}
	first := p.Masked().Addr()
	b := p.Masked()
	size := uint32(1) << uint32(32-b.Bits())
	x := a.As4()
	f := first.As4()
	av := uint32(x[0])<<24 | uint32(x[1])<<16 | uint32(x[2])<<8 | uint32(x[3])
	fv := uint32(f[0])<<24 | uint32(f[1])<<16 | uint32(f[2])<<8 | uint32(f[3])
	return av != fv && av != fv+size-1
}
func validatePrefixes(values []string, where string) error {
	seen := map[netip.Prefix]bool{}
	for _, s := range values {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return fmt.Errorf("%s contains invalid network prefix %q", where, s)
		}
		if seen[p] {
			return fmt.Errorf("%s duplicates %s", where, p)
		}
		seen[p] = true
	}
	return nil
}
func parseDestination(s string) (string, error) {
	if s == "default" {
		return s, nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return "", fmt.Errorf("%q is not an IPv4 network prefix or default", s)
	}
	return p.String(), nil
}
func covered(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) && usableInPrefix(a, p) {
			return true
		}
	}
	return false
}
