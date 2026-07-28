package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Options struct {
	Now          func() time.Time
	GenerationID func() (string, error)
	Namespaces   NamespaceOps
}

type Manager struct {
	run          CommandRunner
	ns           NamespaceOps
	now          func() time.Time
	generationID func() (string, error)
}

func New(runner CommandRunner, opts Options) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.GenerationID == nil {
		opts.GenerationID = randomGenerationID
	}
	if opts.Namespaces == nil {
		opts.Namespaces = OSNamespaceOps{Runner: runner}
	}
	return &Manager{run: runner, ns: opts.Namespaces, now: opts.Now, generationID: opts.GenerationID}
}

func (m *Manager) runIn(ctx context.Context, namespace, program string, args ...string) ([]byte, error) {
	return m.ns.Run(ctx, namespace, program, args...)
}

func (m *Manager) Plan(ctx context.Context, cfg Config) (*Plan, error) {
	if cfg.Topology == "" {
		return nil, errors.New("network: topology name is required")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	p := &Plan{Config: cfg}
	if len(cfg.Externals) == 0 {
		return p, nil
	}
	routes, err := m.hostRoutes(ctx, cfg.ExistingOwnedRoutes)
	if err != nil {
		return nil, fmt.Errorf("network: inspect host routes: %w", err)
	}
	used := make([]netip.Prefix, 0, len(routes))
	for _, route := range routes {
		used = append(used, route.Prefix)
	}
	for _, ext := range cfg.Externals {
		r := ResolvedExternal{External: ext}
		if ext.EgressInterface == "auto" || ext.SourceIP == "auto" {
			dev, src, err := m.routeEgress(ctx)
			if err != nil {
				return nil, fmt.Errorf("network: resolve external %s:%s: %w", ext.Router, ext.Interface, err)
			}
			if ext.EgressInterface == "auto" {
				r.EgressInterface = dev
			} else if ext.EgressInterface != dev {
				// An explicit device may not be selected by the global default;
				// resolve its source independently below.
				src = ""
			}
			if ext.SourceIP == "auto" && src != "" {
				r.SourceIP = src
			}
		}
		if r.EgressInterface == "" || r.EgressInterface == "auto" {
			return nil, fmt.Errorf("network: no egress interface for %s:%s", ext.Router, ext.Interface)
		}
		if ext.SourceIP == "auto" && (r.SourceIP == "" || r.SourceIP == "auto") {
			src, err := m.interfaceSource(ctx, r.EgressInterface)
			if err != nil {
				return nil, fmt.Errorf("network: resolve source on %s: %w", r.EgressInterface, err)
			}
			r.SourceIP = src
		}
		if ext.SourceIP != "auto" {
			if err := m.verifyInterfaceAddress(ctx, r.EgressInterface, ext.SourceIP); err != nil {
				return nil, err
			}
			r.SourceIP = ext.SourceIP
		}
		r.Masquerade = ext.SourceIP == "auto"
		for _, source := range r.Sources {
			prefix, _ := netip.ParsePrefix(source)
			for _, host := range routes {
				if prefixesOverlap(prefix, host.Prefix) {
					return nil, fmt.Errorf("network: external source %s overlaps host route %s", source, host.Prefix)
				}
			}
		}
		if r.TransitPrefix == "" {
			prefix, err := allocateTransit(cfg.Topology, r.Router, r.Interface, used)
			if err != nil {
				return nil, err
			}
			r.TransitPrefix = prefix.String()
		}
		transit, err := netip.ParsePrefix(r.TransitPrefix)
		if err != nil || !transit.Addr().Is4() || transit.Bits() != 30 {
			return nil, fmt.Errorf("network: transit %q must be an IPv4 /30", r.TransitPrefix)
		}
		for _, host := range used {
			if prefixesOverlap(transit, host) {
				return nil, fmt.Errorf("network: transit %s overlaps %s", transit, host)
			}
		}
		used = append(used, transit)
		base := transit.Masked().Addr()
		if r.RouterAddress == "" {
			r.RouterAddress = base.Next().String()
		}
		if r.HostAddress == "" {
			r.HostAddress = base.Next().Next().String()
		}
		p.External = append(p.External, r)
	}
	return p, nil
}

func validateConfig(cfg Config) error {
	seenSwitch := map[string]bool{}
	for _, sw := range cfg.Switches {
		if sw.Name == "" || seenSwitch[sw.Name] {
			return fmt.Errorf("network: duplicate or empty switch %q", sw.Name)
		}
		seenSwitch[sw.Name] = true
		if err := checkMTU(sw.MTU); err != nil {
			return fmt.Errorf("switch %s: %w", sw.Name, err)
		}
	}
	seenRouter := map[string]bool{}
	for _, router := range cfg.Routers {
		if router.Name == "" || seenRouter[router.Name] {
			return fmt.Errorf("network: duplicate or empty router %q", router.Name)
		}
		seenRouter[router.Name] = true
		seenIf := map[string]bool{}
		for _, iface := range router.Interfaces {
			if iface.Name == "" || len(iface.Name) > 15 || seenIf[iface.Name] {
				return fmt.Errorf("router %s: invalid interface %q", router.Name, iface.Name)
			}
			seenIf[iface.Name] = true
			if err := checkMTU(iface.MTU); err != nil {
				return fmt.Errorf("router %s interface %s: %w", router.Name, iface.Name, err)
			}
			for _, vlan := range iface.VLANs {
				if vlan.ID < 1 || vlan.ID > 4094 || vlan.MTU > iface.MTU {
					return fmt.Errorf("router %s interface %s: invalid VLAN %d MTU", router.Name, iface.Name, vlan.ID)
				}
				if len(iface.Name)+1+len(strconv.Itoa(vlan.ID)) > 15 {
					return fmt.Errorf("router %s VLAN interface %s.%d exceeds 15 bytes", router.Name, iface.Name, vlan.ID)
				}
			}
		}
	}
	seenLinks := map[string]bool{}
	attachment := map[string]string{}
	for _, link := range cfg.Links {
		if link.Name == "" || seenLinks[link.Name] {
			return fmt.Errorf("network: duplicate or empty link %q", link.Name)
		}
		seenLinks[link.Name] = true
		if err := checkMTU(link.MTU); err != nil {
			return fmt.Errorf("link %s: %w", link.Name, err)
		}
		a, b := link.Endpoints[0], link.Endpoints[1]
		if a.Node == "" || a.Interface == "" || b.Node == "" || b.Interface == "" {
			return fmt.Errorf("link %s: two complete endpoints are required", link.Name)
		}
		if a.Node == b.Node {
			return fmt.Errorf("link %s: a node cannot link to itself", link.Name)
		}
		for _, ep := range link.Endpoints {
			if err := validateEndpoint(ep, seenSwitch, seenRouter); err != nil {
				return fmt.Errorf("link %s: %w", link.Name, err)
			}
			key := string(ep.Kind) + "\x00" + ep.Node + "\x00" + ep.Interface
			if old := attachment[key]; old != "" {
				return fmt.Errorf("endpoint %s:%s occurs in links %s and %s", ep.Node, ep.Interface, old, link.Name)
			}
			attachment[key] = link.Name
		}
	}
	for _, ext := range cfg.Externals {
		if ext.EgressInterface == "" || ext.SourceIP == "" || len(ext.Sources) == 0 {
			return fmt.Errorf("external %s:%s is incomplete", ext.Router, ext.Interface)
		}
		if err := checkMTU(ext.MTU); err != nil {
			return fmt.Errorf("external %s:%s: %w", ext.Router, ext.Interface, err)
		}
	}
	return nil
}

func validateEndpoint(ep Endpoint, switches, routers map[string]bool) error {
	if err := checkMTU(ep.MTU); err != nil {
		return fmt.Errorf("endpoint %s:%s: %w", ep.Node, ep.Interface, err)
	}
	switch ep.Kind {
	case EndpointVM:
		if ep.Queues < 1 {
			return fmt.Errorf("endpoint %s:%s has invalid queue count", ep.Node, ep.Interface)
		}
	case EndpointRouter:
		if !routers[ep.Node] {
			return fmt.Errorf("unknown router %q", ep.Node)
		}
	case EndpointManagementHost:
	case EndpointSwitch:
		if !switches[ep.Node] {
			return fmt.Errorf("unknown switch %q", ep.Node)
		}
		if ep.Port.Mode != "access" && ep.Port.Mode != "trunk" {
			return fmt.Errorf("switch endpoint %s:%s has invalid port mode", ep.Node, ep.Interface)
		}
	default:
		return fmt.Errorf("endpoint %s:%s has invalid kind %q", ep.Node, ep.Interface, ep.Kind)
	}
	return nil
}

func checkMTU(mtu int) error {
	if mtu < 576 || mtu > 9216 {
		return fmt.Errorf("MTU %d is outside 576..9216", mtu)
	}
	return nil
}

func (m *Manager) Apply(ctx context.Context, p *Plan) (*State, error) {
	return m.apply(ctx, p, nil)
}

// ApplyCheckpoint persists namespace ownership immediately after creation.
// Reconciliation callers should use it when their manifest store is available.
func (m *Manager) ApplyCheckpoint(ctx context.Context, p *Plan, checkpoint func(*State) error) (*State, error) {
	return m.apply(ctx, p, checkpoint)
}

func (m *Manager) apply(ctx context.Context, p *Plan, checkpoint func(*State) error) (*State, error) {
	if p == nil {
		return nil, errors.New("network: nil plan")
	}
	fabricNS := FabricNamespaceName(p.Config.Topology)
	managementNS := ManagementHostNamespaceName(p.Config.Topology)
	state := &State{DatapathVersion: DatapathVersion, Topology: p.Config.Topology, Resources: []Resource{}, Links: map[string]LinkState{}, TAPs: map[string]TAPState{}, ManagementHostInterfaces: map[string]ManagementHostInterface{}, Namespaces: map[string]string{}, NamespaceAliases: map[string]string{}, FabricNamespace: fabricNS, ManagementHostNamespace: managementNS, RouterNFTTables: map[string]string{}, External: append([]ResolvedExternal(nil), p.External...), SharedDir: p.Config.SharedDir}
	for _, item := range []struct{ logical, ns string }{{"fabric", fabricNS}, {"management_host", managementNS}} {
		logical, ns := item.logical, item.ns
		alias := ownerAlias(p.Config.Topology, "namespace", logical)
		state.Resources = append(state.Resources, Resource{Kind: ResourceNamespace, Name: ns, Alias: alias})
		state.Namespaces[logical] = ns
		state.NamespaceAliases[logical] = alias
		if err := m.ensureNamespace(ctx, ns, alias); err != nil {
			return state, err
		}
		if checkpoint != nil {
			if err := checkpoint(state); err != nil {
				return state, err
			}
		}
	}

	switches := append([]Switch(nil), p.Config.Switches...)
	sort.Slice(switches, func(i, j int) bool { return switches[i].Name < switches[j].Name })
	for _, sw := range switches {
		name := DeviceName("tls", p.Config.Topology, sw.Name)
		alias := ownerAlias(p.Config.Topology, "switch", sw.Name)
		state.Resources = append(state.Resources, Resource{Kind: ResourceBridge, Name: name, Namespace: fabricNS, Alias: alias, MTU: sw.MTU})
		if err := m.ensureBridge(ctx, fabricNS, name, alias, sw.MTU, sw.STP, sw.VLANFiltering); err != nil {
			return state, err
		}
	}

	routers := append([]Router(nil), p.Config.Routers...)
	sort.Slice(routers, func(i, j int) bool { return routers[i].Name < routers[j].Name })
	for _, router := range routers {
		ns := namespaceName(p.Config.Topology, router.Name)
		alias := ownerAlias(p.Config.Topology, "namespace", router.Name)
		state.Resources = append(state.Resources, Resource{Kind: ResourceNamespace, Name: ns, Alias: alias})
		if err := m.ensureNamespace(ctx, ns, alias); err != nil {
			return state, err
		}
		state.Namespaces[router.Name] = ns
		state.NamespaceAliases[router.Name] = alias
	}

	links := append([]Link(nil), p.Config.Links...)
	sort.Slice(links, func(i, j int) bool { return links[i].Name < links[j].Name })
	for _, link := range links {
		ls, err := m.applyLink(ctx, p.Config.Topology, link, state)
		if err != nil {
			return state, fmt.Errorf("network: apply link %s: %w", link.Name, err)
		}
		state.Links[link.Name] = ls
	}
	for _, router := range routers {
		if len(router.NAT) > 0 {
			state.RouterNFTTables[router.Name] = DeviceName("tl", p.Config.Topology, router.Name, "nat")
		}
		if err := m.configureRouter(ctx, p.Config.Topology, router, state.Namespaces[router.Name]); err != nil {
			return state, err
		}
	}
	for _, ext := range p.External {
		if err := m.applyExternal(ctx, p.Config.Topology, ext, state); err != nil {
			return state, err
		}
	}
	if len(p.External) > 0 {
		if p.Config.SharedDir == "" {
			return state, errors.New("network: shared directory is required for forwarding ownership")
		}
		guarded, err := m.acquireForwarding(ctx, p.Config.SharedDir, p.Config.Topology, p.External)
		if err != nil {
			return state, err
		}
		state.ForwardingOwner = true
		state.ForwardingGuard = guarded
	}
	if err := m.applyHostNFT(ctx, state); err != nil {
		return state, err
	}
	return state, nil
}

func (m *Manager) applyLink(ctx context.Context, topology string, link Link, state *State) (LinkState, error) {
	fabricNS := state.FabricNamespace
	gen, err := m.generationID()
	if err != nil {
		return LinkState{}, fmt.Errorf("generate datapath ID: %w", err)
	}
	ls := LinkState{Name: link.Name, Bridge: DeviceName("tlb", topology, link.Name), GenerationID: gen, GenerationStartedAt: m.now().UTC()}
	alias := ownerAlias(topology, "link-bridge", link.Name)
	state.Resources = append(state.Resources, Resource{Kind: ResourceBridge, Name: ls.Bridge, Namespace: fabricNS, Alias: alias, MTU: link.MTU})
	if err := m.ensureBridge(ctx, fabricNS, ls.Bridge, alias, link.MTU, false, false); err != nil {
		return ls, err
	}
	for index, ep := range link.Endpoints {
		epKey := fmt.Sprintf("%s:%s", ep.Node, ep.Interface)
		portBridge := DeviceName("tlp", topology, link.Name, strconv.Itoa(index))
		pbAlias := ownerAlias(topology, "port-bridge", link.Name+":"+epKey)
		state.Resources = append(state.Resources, Resource{Kind: ResourceBridge, Name: portBridge, Namespace: fabricNS, Alias: pbAlias, MTU: link.MTU})
		if err := m.ensureBridge(ctx, fabricNS, portBridge, pbAlias, link.MTU, false, false); err != nil {
			return ls, err
		}

		impair := DeviceName("tle", topology, link.Name, strconv.Itoa(index))
		observe := DeviceName("tlo", topology, link.Name, strconv.Itoa(index))
		shimAlias := ownerAlias(topology, "impairment-shim", link.Name+":"+epKey)
		state.Resources = append(state.Resources, Resource{Kind: ResourceLink, Name: impair, Namespace: fabricNS, Alias: shimAlias, MTU: link.MTU})
		if err := m.ensureVeth(ctx, fabricNS, impair, observe, shimAlias, link.MTU, link.MTU); err != nil {
			return ls, err
		}
		if err := m.enslave(ctx, fabricNS, impair, portBridge); err != nil {
			return ls, err
		}
		if err := m.enslave(ctx, fabricNS, observe, ls.Bridge); err != nil {
			return ls, err
		}
		if err := m.applyImpairment(ctx, fabricNS, impair, ep.Egress); err != nil {
			return ls, err
		}
		if err := m.attachEndpoint(ctx, topology, link, index, ep, portBridge, state); err != nil {
			return ls, err
		}
		ls.Endpoints = append(ls.Endpoints, EndpointState{Node: ep.Node, Interface: ep.Interface, IngressDevice: impair, EgressDevice: observe, ImpairmentDevice: impair})
	}
	return ls, nil
}

func (m *Manager) attachEndpoint(ctx context.Context, topology string, link Link, index int, ep Endpoint, portBridge string, state *State) error {
	logical := link.Name + ":" + ep.Node + ":" + ep.Interface
	fabricNS := state.FabricNamespace
	switch ep.Kind {
	case EndpointVM:
		name := DeviceName("tlt", topology, ep.Node, ep.Interface)
		alias := ownerAlias(topology, "tap", ep.Node+":"+ep.Interface)
		state.Resources = append(state.Resources, Resource{Kind: ResourceTAP, Name: name, Namespace: fabricNS, Alias: alias, MTU: link.MTU})
		if err := m.ensureTAP(ctx, fabricNS, name, alias, link.MTU, ep.Queues); err != nil {
			return err
		}
		if err := m.enslave(ctx, fabricNS, name, portBridge); err != nil {
			return err
		}
		state.TAPs[ep.Node+":"+ep.Interface] = TAPState{Node: ep.Node, Interface: ep.Interface, Name: name, Queues: ep.Queues, Alias: alias}
	case EndpointManagementHost, EndpointRouter:
		nodeSide := DeviceName("tln", topology, link.Name, strconv.Itoa(index))
		fabric := DeviceName("tlf", topology, link.Name, strconv.Itoa(index))
		alias := ownerAlias(topology, "endpoint", logical)
		state.Resources = append(state.Resources, Resource{Kind: ResourceLink, Name: fabric, Namespace: fabricNS, Alias: alias, MTU: link.MTU})
		if err := m.ensureVeth(ctx, fabricNS, nodeSide, fabric, alias, ep.MTU, link.MTU); err != nil {
			return err
		}
		if err := m.enslave(ctx, fabricNS, fabric, portBridge); err != nil {
			return err
		}
		if ep.Kind == EndpointManagementHost {
			actual := DeviceName("tlh", topology, ep.Interface)
			if _, err := m.runIn(ctx, fabricNS, "ip", "link", "set", "dev", nodeSide, "netns", state.ManagementHostNamespace); err != nil {
				return err
			}
			if nodeSide != actual {
				if _, err := m.run.Run(ctx, "ip", "-n", state.ManagementHostNamespace, "link", "set", "dev", nodeSide, "name", actual); err != nil {
					return err
				}
			}
			if err := m.setLink(ctx, state.ManagementHostNamespace, actual, ep.MTU, ep.MAC, alias); err != nil {
				return err
			}
			for _, addr := range ep.Addresses {
				if _, err := m.run.Run(ctx, "ip", "-n", state.ManagementHostNamespace, "addr", "replace", addr, "dev", actual); err != nil {
					return err
				}
			}
			state.ManagementHostInterfaces[ep.Interface] = ManagementHostInterface{LogicalName: ep.Interface, DeviceName: actual, Alias: alias, MTU: ep.MTU, Addresses: append([]string(nil), ep.Addresses...), Link: link.Name}
		} else {
			ns := state.Namespaces[ep.Node]
			if ns == "" {
				return fmt.Errorf("router %s has no namespace", ep.Node)
			}
			if _, err := m.runIn(ctx, fabricNS, "ip", "link", "set", "dev", nodeSide, "netns", ns); err != nil {
				return err
			}
			if _, err := m.run.Run(ctx, "ip", "-n", ns, "link", "set", "dev", nodeSide, "name", ep.Interface); err != nil {
				return err
			}
			if err := m.setLink(ctx, ns, ep.Interface, ep.MTU, ep.MAC, alias); err != nil {
				return err
			}
		}
	case EndpointSwitch:
		switchSide := DeviceName("tlw", topology, link.Name, strconv.Itoa(index))
		fabric := DeviceName("tlf", topology, link.Name, strconv.Itoa(index), "switch")
		alias := ownerAlias(topology, "switch-port", logical)
		swBridge := DeviceName("tls", topology, ep.Node)
		state.Resources = append(state.Resources, Resource{Kind: ResourceLink, Name: switchSide, Namespace: fabricNS, Alias: alias, MTU: ep.MTU})
		if err := m.ensureVeth(ctx, fabricNS, switchSide, fabric, alias, ep.MTU, link.MTU); err != nil {
			return err
		}
		if err := m.enslave(ctx, fabricNS, switchSide, swBridge); err != nil {
			return err
		}
		if err := m.enslave(ctx, fabricNS, fabric, portBridge); err != nil {
			return err
		}
		if err := m.configureVLANPort(ctx, fabricNS, switchSide, ep.Port); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) configureRouter(ctx context.Context, topology string, router Router, ns string) error {
	for _, iface := range router.Interfaces {
		alias := ownerAlias(topology, "endpoint", findRouterLogical(topology, router.Name, iface.Name))
		if err := m.setLink(ctx, ns, iface.Name, iface.MTU, iface.MAC, alias); err != nil {
			return fmt.Errorf("router %s interface %s: %w", router.Name, iface.Name, err)
		}
		for _, addr := range iface.Addresses {
			if _, err := m.run.Run(ctx, "ip", "-n", ns, "addr", "replace", addr, "dev", iface.Name); err != nil {
				return err
			}
		}
		vlans := append([]VLAN(nil), iface.VLANs...)
		sort.Slice(vlans, func(i, j int) bool { return vlans[i].ID < vlans[j].ID })
		for _, vlan := range vlans {
			name := iface.Name + "." + strconv.Itoa(vlan.ID)
			if _, err := m.run.Run(ctx, "ip", "-n", ns, "link", "add", "link", iface.Name, "name", name, "type", "vlan", "id", strconv.Itoa(vlan.ID)); err != nil {
				// An existing owned namespace is reconciled in-place. A duplicate
				// VLAN is therefore harmless; subsequent setters still converge it.
			}
			if err := m.setLink(ctx, ns, name, vlan.MTU, "", ownerAlias(topology, "vlan", router.Name+":"+name)); err != nil {
				return err
			}
			for _, addr := range vlan.Addresses {
				if _, err := m.run.Run(ctx, "ip", "-n", ns, "addr", "replace", addr, "dev", name); err != nil {
					return err
				}
			}
		}
	}
	for _, route := range router.Routes {
		to := route.To
		if to == "default" {
			to = "default"
		}
		if _, err := m.run.Run(ctx, "ip", "-n", ns, "route", "replace", to, "via", route.Via, "dev", route.Interface); err != nil {
			return err
		}
	}
	if router.Forwarding || len(router.Interfaces) > 1 || len(router.NAT) > 0 {
		if _, err := m.run.Run(ctx, "ip", "netns", "exec", ns, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1"); err != nil {
			return err
		}
	}
	if err := m.configureNamespaceNAT(ctx, topology, router, ns); err != nil {
		return err
	}
	return m.configureDNSMasq(ctx, topology, router, ns)
}

func findRouterLogical(topology, router, iface string) string {
	// The alias stays deterministic even if the caller builds interfaces before
	// links. The exact link name remains recorded on the root peer.
	return router + ":" + iface
}

func (m *Manager) applyExternal(ctx context.Context, topology string, ext ResolvedExternal, state *State) error {
	alias := ownerAlias(topology, "external", ext.Router+":"+ext.Interface)
	prefix, _ := netip.ParsePrefix(ext.TransitPrefix)
	routerCIDR := ext.RouterAddress + "/" + strconv.Itoa(prefix.Bits())
	hostCIDR := ext.HostAddress + "/" + strconv.Itoa(prefix.Bits())
	routerSide := DeviceName("tlx", topology, ext.Router, ext.Interface)
	hostDevice := DeviceName("tlh", topology, ext.Router, ext.Interface, "external")
	state.Resources = append(state.Resources, Resource{Kind: ResourceLink, Name: hostDevice, Alias: alias, MTU: ext.MTU})
	if err := m.ensureVeth(ctx, "", routerSide, hostDevice, alias, ext.MTU, ext.MTU); err != nil {
		return err
	}
	ns := state.Namespaces[ext.Router]
	if ns == "" {
		return fmt.Errorf("external router %s is absent", ext.Router)
	}
	if _, err := m.run.Run(ctx, "ip", "link", "set", "dev", routerSide, "netns", ns); err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, "ip", "-n", ns, "link", "set", "dev", routerSide, "name", ext.Interface); err != nil {
		return err
	}
	if err := m.setLink(ctx, ns, ext.Interface, ext.MTU, ext.MAC, alias); err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, "ip", "-n", ns, "addr", "replace", routerCIDR, "dev", ext.Interface); err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, "ip", "-n", ns, "route", "replace", "default", "via", ext.HostAddress, "dev", ext.Interface); err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, "ip", "addr", "replace", hostCIDR, "dev", hostDevice); err != nil {
		return err
	}
	for _, source := range ext.Sources {
		if _, err := m.run.Run(ctx, "ip", "route", "replace", source, "via", ext.RouterAddress, "dev", hostDevice, "proto", "static"); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ensureBridge(ctx context.Context, ns, name, alias string, mtu int, stp, vlanFiltering bool) error {
	exists, err := m.ownedLink(ctx, ns, name, alias)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := m.runIn(ctx, ns, "ip", "link", "add", "name", name, "type", "bridge"); err != nil {
			return err
		}
	}
	if _, err := m.runIn(ctx, ns, "ip", "link", "set", "dev", name, "alias", alias); err != nil {
		return err
	}
	if _, err := m.runIn(ctx, ns, "ip", "link", "set", "dev", name, "type", "bridge", "stp_state", bool01(stp), "vlan_filtering", bool01(vlanFiltering)); err != nil {
		return err
	}
	return m.setLink(ctx, ns, name, mtu, "", alias)
}

func (m *Manager) ensureVeth(ctx context.Context, ns, first, second, alias string, firstMTU, secondMTU int) error {
	exists, err := m.ownedLink(ctx, ns, first, alias)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := m.runIn(ctx, ns, "ip", "link", "add", "name", first, "type", "veth", "peer", "name", second); err != nil {
			return err
		}
	}
	for _, name := range []string{first, second} {
		if _, err := m.runIn(ctx, ns, "ip", "link", "set", "dev", name, "alias", alias); err != nil {
			return err
		}
	}
	if err := m.setLink(ctx, ns, first, firstMTU, "", alias); err != nil {
		return err
	}
	return m.setLink(ctx, ns, second, secondMTU, "", alias)
}

func (m *Manager) ensureTAP(ctx context.Context, ns, name, alias string, mtu, queues int) error {
	exists, err := m.ownedLink(ctx, ns, name, alias)
	if err != nil {
		return err
	}
	if !exists {
		args := []string{"tuntap", "add", "dev", name, "mode", "tap"}
		if queues > 1 {
			args = append(args, "multi_queue")
		}
		if _, err := m.runIn(ctx, ns, "ip", args...); err != nil {
			return err
		}
	}
	return m.setLink(ctx, ns, name, mtu, "", alias)
}

func (m *Manager) ensureNamespace(ctx context.Context, ns, alias string) error {
	out, err := m.run.Run(ctx, "ip", "netns", "list")
	exists := err == nil && namespaceListed(string(out), ns)
	if exists {
		owned, err := m.namespaceOwned(ctx, ns, alias)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("network: namespace %s has wrong ownership marker", ns)
		}
	} else {
		if _, err := m.run.Run(ctx, "ip", "netns", "add", ns); err != nil {
			return err
		}
	}
	if _, err := m.run.Run(ctx, "ip", "-n", ns, "link", "set", "dev", "lo", "alias", alias); err != nil {
		return err
	}
	_, err = m.run.Run(ctx, "ip", "-n", ns, "link", "set", "dev", "lo", "up")
	return err
}

func (m *Manager) setLink(ctx context.Context, ns, name string, mtu int, mac, alias string) error {
	prefix := []string{}
	if ns != "" {
		prefix = []string{"-n", ns}
	}
	args := append(prefix, "link", "set", "dev", name, "alias", alias)
	if _, err := m.run.Run(ctx, "ip", args...); err != nil {
		return err
	}
	args = append(prefix, "link", "set", "dev", name, "mtu", strconv.Itoa(mtu))
	if _, err := m.run.Run(ctx, "ip", args...); err != nil {
		return err
	}
	if mac != "" {
		args = append(prefix, "link", "set", "dev", name, "address", mac)
		if _, err := m.run.Run(ctx, "ip", args...); err != nil {
			return err
		}
	}
	args = append(prefix, "link", "set", "dev", name, "up")
	_, err := m.run.Run(ctx, "ip", args...)
	return err
}

func (m *Manager) enslave(ctx context.Context, ns, device, bridge string) error {
	_, err := m.runIn(ctx, ns, "ip", "link", "set", "dev", device, "master", bridge)
	return err
}

func (m *Manager) applyImpairment(ctx context.Context, ns, device string, imp Impairment) error {
	// clsact egress filters run before the root qdisc and therefore provide an
	// owned, cumulative offered counter even for packets later dropped by
	// netem or HTB.
	if _, err := m.runIn(ctx, ns, "tc", "qdisc", "replace", "dev", device, "clsact"); err != nil {
		return err
	}
	if _, err := m.runIn(ctx, ns, "tc", "filter", "replace", "dev", device, "egress", "protocol", "all", "pref", "1", "handle", "1", "matchall", "action", "pass"); err != nil {
		return err
	}
	if imp == (Impairment{}) {
		_, _ = m.runIn(ctx, ns, "tc", "qdisc", "del", "dev", device, "root")
		return nil
	}
	delay := []string{}
	if imp.LatencyMS > 0 || imp.JitterMS > 0 {
		delay = append(delay, "delay", metric(imp.LatencyMS, "ms"))
		if imp.JitterMS > 0 {
			delay = append(delay, metric(imp.JitterMS, "ms"))
		}
	}
	loss := []string{}
	if imp.LossPercent > 0 {
		// The dedicated loss stage does not carry delay and therefore does not
		// intentionally queue packets. Its maximal packet limit keeps its drop
		// counter attributable to configured probabilistic loss. A separate
		// netem stage below owns delay/jitter queue drops.
		loss = []string{"loss", metric(imp.LossPercent, "%"), "limit", "4294967295"}
	}
	attachment := []string{"root"}
	if imp.RateMbps > 0 {
		if _, err := m.runIn(ctx, ns, "tc", "qdisc", "replace", "dev", device, "root", "handle", "1:", "htb", "default", "10"); err != nil {
			return err
		}
		if _, err := m.runIn(ctx, ns, "tc", "class", "replace", "dev", device, "parent", "1:", "classid", "1:10", "htb", "rate", metric(imp.RateMbps, "mbit"), "ceil", metric(imp.RateMbps, "mbit")); err != nil {
			return err
		}
		attachment = []string{"parent", "1:10"}
	}
	if len(loss) > 0 {
		args := append([]string{"qdisc", "replace", "dev", device}, attachment...)
		args = append(args, "handle", "10:", "netem")
		if _, err := m.runIn(ctx, ns, "tc", append(args, loss...)...); err != nil {
			return err
		}
		attachment = []string{"parent", "10:1"}
	}
	if len(delay) > 0 {
		args := append([]string{"qdisc", "replace", "dev", device}, attachment...)
		args = append(args, "handle", "20:", "netem")
		if _, err := m.runIn(ctx, ns, "tc", append(args, delay...)...); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) configureVLANPort(ctx context.Context, ns, device string, port Port) error {
	_, _ = m.runIn(ctx, ns, "bridge", "vlan", "del", "dev", device, "vid", "1")
	if port.Mode == "access" {
		_, err := m.runIn(ctx, ns, "bridge", "vlan", "add", "dev", device, "vid", strconv.Itoa(port.VLAN), "pvid", "untagged")
		return err
	}
	vids := append([]int(nil), port.AllowedVLANs...)
	sort.Ints(vids)
	vids = slices.Compact(vids)
	for _, vid := range vids {
		args := []string{"vlan", "add", "dev", device, "vid", strconv.Itoa(vid)}
		if vid == port.NativeVLAN {
			args = append(args, "pvid", "untagged")
		}
		if _, err := m.runIn(ctx, ns, "bridge", args...); err != nil {
			return err
		}
	}
	return nil
}

func bool01(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
func metric(v float64, suffix string) string { return strconv.FormatFloat(v, 'f', -1, 64) + suffix }

func namespaceListed(output, name string) bool {
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func (m *Manager) ownedLink(ctx context.Context, ns, name, alias string) (bool, error) {
	out, err := m.runIn(ctx, ns, "ip", "-j", "-d", "link", "show", "dev", name)
	if err != nil {
		if commandReportsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("network: inspect interface %s: %w", name, err)
	}
	var links []struct {
		Alias string `json:"ifalias"`
	}
	if err := json.Unmarshal(out, &links); err != nil {
		return false, fmt.Errorf("network: decode interface %s: %w", name, err)
	}
	if len(links) != 1 {
		return false, fmt.Errorf("network: unexpected interface result for %s", name)
	}
	if links[0].Alias != alias {
		return false, fmt.Errorf("network: interface %s is not owned (alias %q)", name, links[0].Alias)
	}
	return true, nil
}

func (m *Manager) namespaceOwned(ctx context.Context, ns, alias string) (bool, error) {
	out, err := m.run.Run(ctx, "ip", "-n", ns, "-j", "-d", "link", "show", "dev", "lo")
	if err != nil {
		return false, err
	}
	var links []struct {
		Alias string `json:"ifalias"`
	}
	if err := json.Unmarshal(out, &links); err != nil {
		return false, err
	}
	return len(links) == 1 && links[0].Alias == alias, nil
}
