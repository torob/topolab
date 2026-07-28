package config

import (
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

func collectOrder(doc *yaml.Node) declarationOrder {
	o := declarationOrder{
		VMDisks: make(map[string][]string), VMInterfaces: make(map[string][]string),
		VMVLANs: make(map[string]map[string][]int), RouterIfaces: make(map[string][]string),
		RouterVLANs: make(map[string]map[string][]int),
	}
	root := documentMapping(doc)
	o.Templates = stringKeys(mappingValue(root, "templates"))
	o.ManagementHostInterfaces = stringKeys(mappingValue(mappingValue(root, "management_host"), "interfaces"))
	vms := mappingValue(root, "vms")
	o.VMs = stringKeys(vms)
	for i := 0; vms != nil && i < len(vms.Content); i += 2 {
		name, vm := vms.Content[i].Value, vms.Content[i+1]
		o.VMDisks[name] = stringKeys(mappingValue(vm, "additional_disks"))
		ifaces := mappingValue(vm, "interfaces")
		o.VMInterfaces[name] = stringKeys(ifaces)
		o.VMVLANs[name] = make(map[string][]int)
		for j := 0; ifaces != nil && j < len(ifaces.Content); j += 2 {
			o.VMVLANs[name][ifaces.Content[j].Value] = intKeys(mappingValue(ifaces.Content[j+1], "vlans"))
		}
	}
	routers := mappingValue(root, "routers")
	o.Routers = stringKeys(routers)
	for i := 0; routers != nil && i < len(routers.Content); i += 2 {
		name, router := routers.Content[i].Value, routers.Content[i+1]
		ifaces := mappingValue(router, "interfaces")
		o.RouterIfaces[name] = stringKeys(ifaces)
		o.RouterVLANs[name] = make(map[string][]int)
		for j := 0; ifaces != nil && j < len(ifaces.Content); j += 2 {
			o.RouterVLANs[name][ifaces.Content[j].Value] = intKeys(mappingValue(ifaces.Content[j+1], "vlans"))
		}
	}
	o.Switches = stringKeys(mappingValue(root, "switches"))
	o.Links = stringKeys(mappingValue(root, "links"))
	return o
}

func documentMapping(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		return n.Content[0]
	}
	return n
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	n = documentMapping(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func stringKeys(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	r := make([]string, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		r = append(r, n.Content[i].Value)
	}
	return r
}

func intKeys(n *yaml.Node) []int {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	r := make([]int, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		var v int
		if n.Content[i].Decode(&v) == nil {
			r = append(r, v)
		}
	}
	return r
}

// Marshal emits normalized YAML while retaining source order for named maps.
func Marshal(cfg *Config) ([]byte, error) {
	var doc yaml.Node
	if err := doc.Encode(cfg); err != nil {
		return nil, err
	}
	root := documentMapping(&doc)
	reorder(mappingValue(root, "templates"), cfg.order.Templates)
	reorder(mappingValue(mappingValue(root, "management_host"), "interfaces"), cfg.order.ManagementHostInterfaces)
	vms := mappingValue(root, "vms")
	reorder(vms, cfg.order.VMs)
	for _, name := range cfg.order.VMs {
		vm := mappingValue(vms, name)
		reorder(mappingValue(vm, "additional_disks"), cfg.order.VMDisks[name])
		ifaces := mappingValue(vm, "interfaces")
		reorder(ifaces, cfg.order.VMInterfaces[name])
		for _, iface := range cfg.order.VMInterfaces[name] {
			reorderInts(mappingValue(mappingValue(ifaces, iface), "vlans"), cfg.order.VMVLANs[name][iface])
		}
	}
	routers := mappingValue(root, "routers")
	reorder(routers, cfg.order.Routers)
	for _, name := range cfg.order.Routers {
		ifaces := mappingValue(mappingValue(routers, name), "interfaces")
		reorder(ifaces, cfg.order.RouterIfaces[name])
		for _, iface := range cfg.order.RouterIfaces[name] {
			reorderInts(mappingValue(mappingValue(ifaces, iface), "vlans"), cfg.order.RouterVLANs[name][iface])
		}
	}
	reorder(mappingValue(root, "switches"), cfg.order.Switches)
	reorder(mappingValue(root, "links"), cfg.order.Links)
	return yaml.Marshal(&doc)
}

func reorder(n *yaml.Node, keys []string) {
	if n == nil || n.Kind != yaml.MappingNode || len(keys) == 0 {
		return
	}
	pairs := make(map[string][2]*yaml.Node, len(n.Content)/2)
	original := make([]string, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		pairs[n.Content[i].Value] = [2]*yaml.Node{n.Content[i], n.Content[i+1]}
		original = append(original, n.Content[i].Value)
	}
	n.Content = n.Content[:0]
	for _, key := range keys {
		if p, ok := pairs[key]; ok {
			n.Content = append(n.Content, p[0], p[1])
			delete(pairs, key)
		}
	}
	for _, key := range original {
		if p, ok := pairs[key]; ok {
			n.Content = append(n.Content, p[0], p[1])
		}
	}
}

func reorderInts(n *yaml.Node, keys []int) {
	ss := make([]string, len(keys))
	for i, k := range keys {
		ss[i] = yamlInt(k)
	}
	reorder(n, ss)
}

func yamlInt(v int) string {
	return strconv.Itoa(v)
}

// The order accessors expose declaration order to runtime generators. They
// return copies so callers cannot mutate parser state.
func (c *Config) VMNames() []string     { return ordered(c.order.VMs, c.VMs) }
func (c *Config) RouterNames() []string { return ordered(c.order.Routers, c.Routers) }
func (c *Config) SwitchNames() []string { return ordered(c.order.Switches, c.Switches) }
func (c *Config) LinkNames() []string   { return ordered(c.order.Links, c.Links) }
func (c *Config) ManagementHostInterfaceNames() []string {
	return ordered(c.order.ManagementHostInterfaces, c.ManagementHost.Interfaces)
}
func (c *Config) VMInterfaceNames(vm string) []string {
	return ordered(c.order.VMInterfaces[vm], c.VMs[vm].Interfaces)
}
func (c *Config) VMAdditionalDiskNames(vm string) []string {
	return ordered(c.order.VMDisks[vm], c.VMs[vm].AdditionalDisks)
}
func (c *Config) VMVLANIDs(vm, iface string) []int {
	return orderedInts(c.order.VMVLANs[vm][iface], c.VMs[vm].Interfaces[iface].VLANs)
}
func (c *Config) RouterInterfaceNames(router string) []string {
	return ordered(c.order.RouterIfaces[router], c.Routers[router].Interfaces)
}
func (c *Config) RouterVLANIDs(router, iface string) []int {
	return orderedInts(c.order.RouterVLANs[router][iface], c.Routers[router].Interfaces[iface].VLANs)
}

func ordered[T any](preferred []string, values map[string]T) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, name := range preferred {
		if _, ok := values[name]; ok && !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	var rest []string
	for name := range values {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

func orderedInts[T any](preferred []int, values map[int]T) []int {
	seen := make(map[int]bool, len(values))
	out := make([]int, 0, len(values))
	for _, id := range preferred {
		if _, ok := values[id]; ok && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	var rest []int
	for id := range values {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}
