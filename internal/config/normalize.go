package config

import (
	"fmt"
	"os"
	"path/filepath"
)

func normalize(c *Config, baseDir string) error {
	if baseDir == "" {
		baseDir = "."
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return fmt.Errorf("resolve topology directory: %w", err)
	}
	ensureMaps(c)
	for name, path := range c.Templates {
		if !filepath.IsAbs(path) {
			path = filepath.Join(absBase, path)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("template %q: resolve path: %w", name, err)
		}
		c.Templates[name] = filepath.Clean(path)
	}
	for name, h := range c.ManagementHost.Interfaces {
		if h == nil {
			h = &ManagementHostInterface{}
			c.ManagementHost.Interfaces[name] = h
		}
		if h.MTU == 0 {
			h.MTU = 1500
		}
		h.MAC = DeterministicMAC(c.Name, "management_host", name)
		if h.IPv4.Addresses == nil {
			h.IPv4.Addresses = []string{}
		}
	}
	for name, vm := range c.VMs {
		if vm == nil {
			vm = &VM{}
			c.VMs[name] = vm
		}
		if vm.MemoryMB == 0 {
			vm.MemoryMB = 2048
		}
		if vm.CPU == nil {
			vm.CPU = &CPU{}
		}
		if vm.CPU.Model == "" {
			vm.CPU.Model = "host"
		}
		one, two := 1, 2
		if vm.CPU.Sockets == nil {
			vm.CPU.Sockets = &one
		}
		if vm.CPU.Cores == nil {
			vm.CPU.Cores = &two
		}
		if vm.CPU.Threads == nil {
			vm.CPU.Threads = &one
		}
		if vm.MemoryBalloon == nil {
			vm.MemoryBalloon = &MemoryBalloon{}
		}
		if vm.MemoryBalloon.Enabled && vm.MemoryBalloon.TargetMB == nil {
			target := vm.MemoryMB
			vm.MemoryBalloon.TargetMB = &target
		}
		if vm.RootDisk.Bus == "" {
			vm.RootDisk.Bus = "scsi"
		}
		if vm.AdditionalDisks == nil {
			vm.AdditionalDisks = map[string]*Disk{}
		}
		for diskName, disk := range vm.AdditionalDisks {
			if disk == nil {
				disk = &Disk{}
				vm.AdditionalDisks[diskName] = disk
			}
			if disk.Bus == "" {
				disk.Bus = "scsi"
			}
		}
		if len(vm.Boot.Order) == 0 {
			vm.Boot.Order = []string{"disk"}
		}
		if vm.SSHAuthorizedKeys == nil {
			vm.SSHAuthorizedKeys = []string{}
		}
		if vm.Interfaces == nil {
			vm.Interfaces = map[string]*VMInterface{}
		}
		for ifaceName, iface := range vm.Interfaces {
			normalizeVMInterface(c.Name, name, ifaceName, iface, vm.Interfaces)
		}
	}
	for name, r := range c.Routers {
		if r == nil {
			r = &Router{}
			c.Routers[name] = r
		}
		if r.Interfaces == nil {
			r.Interfaces = map[string]*RouterInterface{}
		}
		if r.Routes == nil {
			r.Routes = []RouterRoute{}
		}
		for ifaceName, iface := range r.Interfaces {
			normalizeRouterInterface(c.Name, name, ifaceName, iface, r.Interfaces)
		}
	}
	for name, sw := range c.Switches {
		if sw == nil {
			sw = &Switch{}
			c.Switches[name] = sw
		}
		if sw.MTU == 0 {
			sw.MTU = 1500
		}
	}
	for name, link := range c.Links {
		if link == nil {
			link = &Link{}
			c.Links[name] = link
		}
		if link.MTU == 0 {
			link.MTU = 1500
		}
		if link.Endpoints == nil {
			link.Endpoints = []*Endpoint{}
		}
		for _, ep := range link.Endpoints {
			if ep == nil {
				continue
			}
			if _, ok := c.Switches[ep.Node]; ok {
				if ep.Port == nil {
					vlan := 1
					ep.Port = &Port{Mode: "access", VLAN: &vlan}
				}
				if ep.Port.Mode == "" {
					ep.Port.Mode = "access"
				}
				if ep.Port.Mode == "access" && ep.Port.VLAN == nil {
					vlan := 1
					ep.Port.VLAN = &vlan
				}
			}
		}
	}
	for name, path := range c.Templates {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("template %q path %q: %w", name, path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("template %q path %q is not a regular file", name, path)
		}
	}
	return nil
}

func ensureMaps(c *Config) {
	if c.Templates == nil {
		c.Templates = map[string]string{}
	}
	if c.ManagementHost.Interfaces == nil {
		c.ManagementHost.Interfaces = map[string]*ManagementHostInterface{}
	}
	if c.VMs == nil {
		c.VMs = map[string]*VM{}
	}
	if c.Routers == nil {
		c.Routers = map[string]*Router{}
	}
	if c.Switches == nil {
		c.Switches = map[string]*Switch{}
	}
	if c.Links == nil {
		c.Links = map[string]*Link{}
	}
}

func normalizeVMInterface(topology, node, name string, iface *VMInterface, all map[string]*VMInterface) {
	if iface == nil {
		iface = &VMInterface{}
		all[name] = iface
	}
	if iface.Model == "" {
		iface.Model = "virtio"
	}
	if iface.Queues == 0 {
		iface.Queues = 1
	}
	if iface.MAC == "" {
		iface.MAC = DeterministicMAC(topology, node, name)
	}
	if iface.MTU == 0 {
		iface.MTU = 1500
	}
	if iface.VLANs == nil {
		iface.VLANs = map[int]*VMVLAN{}
	}
	normalizeVMIPv4(iface.IPv4)
	for id, vlan := range iface.VLANs {
		if vlan == nil {
			vlan = &VMVLAN{}
			iface.VLANs[id] = vlan
		}
		if vlan.MTU == 0 {
			vlan.MTU = iface.MTU
		}
		normalizeVMIPv4(vlan.IPv4)
	}
}

func normalizeVMIPv4(ip *VMIPv4) {
	if ip == nil {
		return
	}
	if ip.Addresses == nil {
		ip.Addresses = []string{}
	}
	if ip.Routes == nil {
		ip.Routes = []VMRoute{}
	}
	if ip.DNSServers == nil {
		ip.DNSServers = []string{}
	}
}

func normalizeRouterInterface(topology, node, name string, iface *RouterInterface, all map[string]*RouterInterface) {
	if iface == nil {
		iface = &RouterInterface{}
		all[name] = iface
	}
	if iface.MAC == "" {
		iface.MAC = DeterministicMAC(topology, node, name)
	}
	if iface.MTU == 0 {
		iface.MTU = 1500
	}
	if iface.VLANs == nil {
		iface.VLANs = map[int]*RouterVLAN{}
	}
	normalizeRouterIPv4(iface.IPv4)
	for id, vlan := range iface.VLANs {
		if vlan == nil {
			vlan = &RouterVLAN{}
			iface.VLANs[id] = vlan
		}
		if vlan.MTU == 0 {
			vlan.MTU = iface.MTU
		}
		normalizeRouterIPv4(vlan.IPv4)
	}
	if iface.External != nil {
		if iface.External.Sources == nil {
			iface.External.Sources = []string{}
		}
		if iface.External.EgressInterface == "" {
			iface.External.EgressInterface = "auto"
		}
		if iface.External.SourceIP == "" {
			iface.External.SourceIP = "auto"
		}
	}
	if iface.NAT != nil && iface.NAT.Sources == nil {
		iface.NAT.Sources = []string{}
	}
}

func normalizeRouterIPv4(ip *RouterIPv4) {
	if ip == nil {
		return
	}
	if ip.Addresses == nil {
		ip.Addresses = []string{}
	}
	if ip.DHCPServer != nil {
		if ip.DHCPServer.Lease == "" {
			ip.DHCPServer.Lease = "12h"
		}
		if ip.DHCPServer.DNSServers == nil {
			ip.DHCPServer.DNSServers = []string{}
		}
	}
}
