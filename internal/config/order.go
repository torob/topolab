package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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

func documentMapping(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		return n.Content[0]
	}
	return n
}

type configIndex struct {
	templates                map[string]*Template
	storages                 map[string]*Storage
	managementHostInterfaces map[string]*ManagementHostInterface
	vms                      map[string]*VM
	vmDisks                  map[string]map[string]*Disk
	vmInterfaces             map[string]map[string]*VMInterface
	vmVLANs                  map[string]map[string]map[int]*VMVLAN
	routers                  map[string]*Router
	routerInterfaces         map[string]map[string]*RouterInterface
	routerVLANs              map[string]map[string]map[int]*RouterVLAN
	switches                 map[string]*Switch
	links                    map[string]*Link
}

func buildIndexes(c *Config) error {
	idx := &configIndex{
		templates: map[string]*Template{}, storages: map[string]*Storage{}, managementHostInterfaces: map[string]*ManagementHostInterface{},
		vms: map[string]*VM{}, vmDisks: map[string]map[string]*Disk{}, vmInterfaces: map[string]map[string]*VMInterface{}, vmVLANs: map[string]map[string]map[int]*VMVLAN{},
		routers: map[string]*Router{}, routerInterfaces: map[string]map[string]*RouterInterface{}, routerVLANs: map[string]map[string]map[int]*RouterVLAN{},
		switches: map[string]*Switch{}, links: map[string]*Link{},
	}
	if err := indexNamed("template", c.Templates, func(v *Template) string { return v.Name }, idx.templates); err != nil {
		return err
	}
	if err := indexNamed("storage", c.Storages, func(v *Storage) string { return v.Name }, idx.storages); err != nil {
		return err
	}
	if err := indexNamed("management-host interface", c.ManagementHost.Interfaces, func(v *ManagementHostInterface) string { return v.Name }, idx.managementHostInterfaces); err != nil {
		return err
	}
	if err := indexNamed("VM", c.VMs, func(v *VM) string { return v.Name }, idx.vms); err != nil {
		return err
	}
	for _, vm := range c.VMs {
		idx.vmDisks[vm.Name] = map[string]*Disk{}
		if err := indexNamed(fmt.Sprintf("VM %q disk", vm.Name), vm.AdditionalDisks, func(v *Disk) string { return v.Name }, idx.vmDisks[vm.Name]); err != nil {
			return err
		}
		idx.vmInterfaces[vm.Name] = map[string]*VMInterface{}
		idx.vmVLANs[vm.Name] = map[string]map[int]*VMVLAN{}
		if err := indexNamed(fmt.Sprintf("VM %q interface", vm.Name), vm.Interfaces, func(v *VMInterface) string { return v.Name }, idx.vmInterfaces[vm.Name]); err != nil {
			return err
		}
		for _, iface := range vm.Interfaces {
			idx.vmVLANs[vm.Name][iface.Name] = map[int]*VMVLAN{}
			if err := indexVLANs(fmt.Sprintf("VM %q interface %q", vm.Name, iface.Name), iface.VLANs, func(v *VMVLAN) int { return v.ID }, idx.vmVLANs[vm.Name][iface.Name]); err != nil {
				return err
			}
		}
	}
	if err := indexNamed("router", c.Routers, func(v *Router) string { return v.Name }, idx.routers); err != nil {
		return err
	}
	for _, router := range c.Routers {
		idx.routerInterfaces[router.Name] = map[string]*RouterInterface{}
		idx.routerVLANs[router.Name] = map[string]map[int]*RouterVLAN{}
		if err := indexNamed(fmt.Sprintf("router %q interface", router.Name), router.Interfaces, func(v *RouterInterface) string { return v.Name }, idx.routerInterfaces[router.Name]); err != nil {
			return err
		}
		for _, iface := range router.Interfaces {
			idx.routerVLANs[router.Name][iface.Name] = map[int]*RouterVLAN{}
			if err := indexVLANs(fmt.Sprintf("router %q interface %q", router.Name, iface.Name), iface.VLANs, func(v *RouterVLAN) int { return v.ID }, idx.routerVLANs[router.Name][iface.Name]); err != nil {
				return err
			}
		}
	}
	if err := indexNamed("switch", c.Switches, func(v *Switch) string { return v.Name }, idx.switches); err != nil {
		return err
	}
	if err := indexNamed("link", c.Links, func(v *Link) string { return v.Name }, idx.links); err != nil {
		return err
	}
	c.index = idx
	return nil
}

func indexNamed[T any](kind string, values []*T, identity func(*T) string, dst map[string]*T) error {
	for i, value := range values {
		if value == nil {
			return fmt.Errorf("%s entry %d is null", kind, i)
		}
		name := identity(value)
		if name == "" {
			return fmt.Errorf("%s entry %d is missing name", kind, i)
		}
		if _, exists := dst[name]; exists {
			return fmt.Errorf("duplicate %s name %q", kind, name)
		}
		dst[name] = value
	}
	return nil
}

func indexVLANs[T any](kind string, values []*T, identity func(*T) int, dst map[int]*T) error {
	for i, value := range values {
		if value == nil {
			return fmt.Errorf("%s VLAN entry %d is null", kind, i)
		}
		id := identity(value)
		if id == 0 {
			return fmt.Errorf("%s VLAN entry %d is missing id", kind, i)
		}
		if _, exists := dst[id]; exists {
			return fmt.Errorf("%s duplicates VLAN id %d", kind, id)
		}
		dst[id] = value
	}
	return nil
}

// Marshal serializes only the authoritative slices, in their declared order.
func Marshal(cfg *Config) ([]byte, error) { return yaml.Marshal(cfg) }

func (c *Config) LookupTemplate(name string) (*Template, bool) {
	if c.index != nil {
		v, ok := c.index.templates[name]
		return v, ok
	}
	return findNamed(c.Templates, name, func(v *Template) string { return v.Name })
}
func (c *Config) LookupStorage(name string) (*Storage, bool) {
	if c.index != nil {
		v, ok := c.index.storages[name]
		return v, ok
	}
	return findNamed(c.Storages, name, func(v *Storage) string { return v.Name })
}
func (c *Config) LookupManagementHostInterface(name string) (*ManagementHostInterface, bool) {
	if c.index != nil {
		v, ok := c.index.managementHostInterfaces[name]
		return v, ok
	}
	return findNamed(c.ManagementHost.Interfaces, name, func(v *ManagementHostInterface) string { return v.Name })
}
func (c *Config) LookupVM(name string) (*VM, bool) {
	if c.index != nil {
		v, ok := c.index.vms[name]
		return v, ok
	}
	return findNamed(c.VMs, name, func(v *VM) string { return v.Name })
}
func (c *Config) LookupRouter(name string) (*Router, bool) {
	if c.index != nil {
		v, ok := c.index.routers[name]
		return v, ok
	}
	return findNamed(c.Routers, name, func(v *Router) string { return v.Name })
}
func (c *Config) LookupSwitch(name string) (*Switch, bool) {
	if c.index != nil {
		v, ok := c.index.switches[name]
		return v, ok
	}
	return findNamed(c.Switches, name, func(v *Switch) string { return v.Name })
}
func (c *Config) LookupLink(name string) (*Link, bool) {
	if c.index != nil {
		v, ok := c.index.links[name]
		return v, ok
	}
	return findNamed(c.Links, name, func(v *Link) string { return v.Name })
}

func (c *Config) LookupVMInterface(vm, name string) (*VMInterface, bool) {
	if c.index != nil {
		v, ok := c.index.vmInterfaces[vm][name]
		return v, ok
	}
	parent, ok := c.LookupVM(vm)
	if !ok {
		return nil, false
	}
	return findNamed(parent.Interfaces, name, func(v *VMInterface) string { return v.Name })
}
func (c *Config) LookupVMAdditionalDisk(vm, name string) (*Disk, bool) {
	if c.index != nil {
		v, ok := c.index.vmDisks[vm][name]
		return v, ok
	}
	parent, ok := c.LookupVM(vm)
	if !ok {
		return nil, false
	}
	return findNamed(parent.AdditionalDisks, name, func(v *Disk) string { return v.Name })
}
func (c *Config) LookupVMVLAN(vm, iface string, id int) (*VMVLAN, bool) {
	if c.index != nil {
		v, ok := c.index.vmVLANs[vm][iface][id]
		return v, ok
	}
	parent, ok := c.LookupVMInterface(vm, iface)
	if !ok {
		return nil, false
	}
	return findID(parent.VLANs, id, func(v *VMVLAN) int { return v.ID })
}
func (c *Config) LookupRouterInterface(router, name string) (*RouterInterface, bool) {
	if c.index != nil {
		v, ok := c.index.routerInterfaces[router][name]
		return v, ok
	}
	parent, ok := c.LookupRouter(router)
	if !ok {
		return nil, false
	}
	return findNamed(parent.Interfaces, name, func(v *RouterInterface) string { return v.Name })
}
func (c *Config) LookupRouterVLAN(router, iface string, id int) (*RouterVLAN, bool) {
	if c.index != nil {
		v, ok := c.index.routerVLANs[router][iface][id]
		return v, ok
	}
	parent, ok := c.LookupRouterInterface(router, iface)
	if !ok {
		return nil, false
	}
	return findID(parent.VLANs, id, func(v *RouterVLAN) int { return v.ID })
}

func findNamed[T any](values []*T, name string, identity func(*T) string) (*T, bool) {
	for _, v := range values {
		if v != nil && identity(v) == name {
			return v, true
		}
	}
	return nil, false
}
func findID[T any](values []*T, id int, identity func(*T) int) (*T, bool) {
	for _, v := range values {
		if v != nil && identity(v) == id {
			return v, true
		}
	}
	return nil, false
}

func (c *Config) VMNames() []string { return names(c.VMs, func(v *VM) string { return v.Name }) }
func (c *Config) StorageNames() []string {
	return names(c.Storages, func(v *Storage) string { return v.Name })
}
func (c *Config) RouterNames() []string {
	return names(c.Routers, func(v *Router) string { return v.Name })
}
func (c *Config) SwitchNames() []string {
	return names(c.Switches, func(v *Switch) string { return v.Name })
}
func (c *Config) LinkNames() []string { return names(c.Links, func(v *Link) string { return v.Name }) }
func (c *Config) ManagementHostInterfaceNames() []string {
	return names(c.ManagementHost.Interfaces, func(v *ManagementHostInterface) string { return v.Name })
}
func (c *Config) VMInterfaceNames(vm string) []string {
	v, ok := c.LookupVM(vm)
	if !ok {
		return nil
	}
	return names(v.Interfaces, func(v *VMInterface) string { return v.Name })
}
func (c *Config) VMAdditionalDiskNames(vm string) []string {
	v, ok := c.LookupVM(vm)
	if !ok {
		return nil
	}
	return names(v.AdditionalDisks, func(v *Disk) string { return v.Name })
}
func (c *Config) VMVLANIDs(vm, iface string) []int {
	v, ok := c.LookupVMInterface(vm, iface)
	if !ok {
		return nil
	}
	return ids(v.VLANs, func(v *VMVLAN) int { return v.ID })
}
func (c *Config) RouterInterfaceNames(router string) []string {
	v, ok := c.LookupRouter(router)
	if !ok {
		return nil
	}
	return names(v.Interfaces, func(v *RouterInterface) string { return v.Name })
}
func (c *Config) RouterVLANIDs(router, iface string) []int {
	v, ok := c.LookupRouterInterface(router, iface)
	if !ok {
		return nil
	}
	return ids(v.VLANs, func(v *RouterVLAN) int { return v.ID })
}
func names[T any](values []*T, identity func(*T) string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != nil {
			out = append(out, identity(v))
		}
	}
	return out
}
func ids[T any](values []*T, identity func(*T) int) []int {
	out := make([]int, 0, len(values))
	for _, v := range values {
		if v != nil {
			out = append(out, identity(v))
		}
	}
	return out
}
