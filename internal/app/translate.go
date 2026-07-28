package app

import (
	"slices"
	"strings"

	"github.com/torob/topolab/internal/cloudinit"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
)

func translateNetwork(cfg *config.Config, shared string, prior *network.State) network.Config {
	out := network.Config{Topology: cfg.Name, SharedDir: shared}
	if prior != nil {
		for _, ext := range prior.External {
			device := externalHostDevice(cfg.Name, ext.External)
			marker := resourceMarker(prior, device)
			if marker == "" {
				continue
			}
			out.ExistingOwnedRoutes = append(out.ExistingOwnedRoutes, network.OwnedRoute{Prefix: ext.TransitPrefix, Device: device, Protocol: "kernel", Marker: marker})
			for _, source := range ext.Sources {
				out.ExistingOwnedRoutes = append(out.ExistingOwnedRoutes, network.OwnedRoute{Prefix: source, Device: device, Via: ext.RouterAddress, Protocol: "static", Marker: marker})
			}
		}
		slices.SortFunc(out.ExistingOwnedRoutes, func(a, b network.OwnedRoute) int {
			return strings.Compare(a.Prefix+"\x00"+a.Device+"\x00"+a.Via, b.Prefix+"\x00"+b.Device+"\x00"+b.Via)
		})
	}
	for _, s := range cfg.Switches {
		out.Switches = append(out.Switches, network.Switch{Name: s.Name, MTU: s.MTU, STP: s.STP, VLANFiltering: s.VLANFiltering})
	}
	for _, r := range cfg.Routers {
		name := r.Name
		nr := network.Router{Name: name}
		for _, iface := range r.Interfaces {
			if iface.External != nil {
				nr.Forwarding = true
			}
		}
		for _, i := range r.Interfaces {
			in := i.Name
			if i.External != nil {
				continue
			}
			ri := network.RouterInterface{Name: in, MTU: i.MTU, MAC: i.MAC}
			if i.IPv4 != nil {
				ri.Addresses = append([]string(nil), i.IPv4.Addresses...)
				appendDHCP(&nr, in, i.IPv4)
			}
			for _, v := range i.VLANs {
				id := v.ID
				nv := network.VLAN{ID: id, MTU: v.MTU}
				if v.IPv4 != nil {
					nv.Addresses = append([]string(nil), v.IPv4.Addresses...)
					appendDHCP(&nr, in+"."+itoa(id), v.IPv4)
				}
				ri.VLANs = append(ri.VLANs, nv)
			}
			nr.Interfaces = append(nr.Interfaces, ri)
			if i.NAT != nil {
				nr.NAT = append(nr.NAT, network.NAT{Interface: in, Sources: append([]string(nil), i.NAT.Sources...)})
			}
		}
		for _, route := range r.Routes {
			nr.Routes = append(nr.Routes, network.Route{To: route.To, Via: route.Via, Interface: route.Interface})
		}
		out.Routers = append(out.Routers, nr)
		for _, i := range r.Interfaces {
			in := i.Name
			if i.External == nil {
				continue
			}
			e := network.External{Router: name, Interface: in, MAC: i.MAC, MTU: i.MTU, EgressInterface: i.External.EgressInterface, SourceIP: i.External.SourceIP, Sources: append([]string(nil), i.External.Sources...)}
			if prior != nil {
				for _, p := range prior.External {
					if p.Router == name && p.Interface == in {
						e.TransitPrefix, e.RouterAddress, e.HostAddress = p.TransitPrefix, p.RouterAddress, p.HostAddress
					}
				}
			}
			out.Externals = append(out.Externals, e)
		}
	}
	for _, l := range cfg.Links {
		nl := network.Link{Name: l.Name, MTU: l.MTU}
		for i, ep := range l.Endpoints {
			nl.Endpoints[i] = translateEndpoint(cfg, ep, l.MTU)
		}
		out.Links = append(out.Links, nl)
	}
	return out
}

func resourceMarker(value *network.State, device string) string {
	for _, resource := range value.Resources {
		if resource.Name == device {
			return resource.Alias
		}
	}
	return ""
}

func externalHostDevice(topology string, ext network.External) string {
	return network.DeviceName("tlh", topology, ext.Router, ext.Interface, "external")
}

type canonicalNetwork struct {
	Config   network.Config
	External []network.ResolvedExternal
}

// canonicalNetworkConfig removes manifest-only overlap context and retains the
// exact allocation, auto-resolution, and masquerade-versus-SNAT decision.
func canonicalNetworkConfig(cfg network.Config, resolved []network.ResolvedExternal) canonicalNetwork {
	cfg.ExistingOwnedRoutes = nil
	cfg.Externals = nil
	return canonicalNetwork{Config: cfg, External: append([]network.ResolvedExternal(nil), resolved...)}
}

func appendDHCP(router *network.Router, name string, ip *config.RouterIPv4) {
	if ip == nil || ip.DHCPServer == nil {
		return
	}
	d := ip.DHCPServer
	router.DHCP = append(router.DHCP, network.DHCPServer{Interface: name, RangeStart: d.RangeStart, RangeEnd: d.RangeEnd, Lease: d.Lease, DNSServers: append([]string(nil), d.DNSServers...)})
}

func translateEndpoint(cfg *config.Config, ep *config.Endpoint, linkMTU int) network.Endpoint {
	e := network.Endpoint{Node: ep.Node, Interface: ep.Interface, MTU: linkMTU, Queues: 1}
	if ep.Egress != nil {
		e.Egress = network.Impairment{LatencyMS: ep.Egress.LatencyMS, JitterMS: ep.Egress.JitterMS, LossPercent: ep.Egress.LossPercent, RateMbps: ep.Egress.RateMbps}
	}
	if ep.Node == "management_host" {
		h, _ := cfg.LookupManagementHostInterface(ep.Interface)
		e.Kind, e.MTU, e.MAC, e.Addresses = network.EndpointManagementHost, h.MTU, h.MAC, append([]string(nil), h.IPv4.Addresses...)
	} else if v, ok := cfg.LookupVMInterface(ep.Node, ep.Interface); ok {
		e.Kind, e.MAC, e.Queues = network.EndpointVM, v.MAC, v.Queues
	} else if v, ok := cfg.LookupRouterInterface(ep.Node, ep.Interface); ok {
		e.MTU, e.MAC = v.MTU, v.MAC
		e.Kind = network.EndpointRouter
	} else if s, ok := cfg.LookupSwitch(ep.Node); ok {
		e.Kind, e.MTU = network.EndpointSwitch, s.MTU
		if ep.Port != nil {
			e.Port.Mode, e.Port.AllowedVLANs = ep.Port.Mode, append([]int(nil), ep.Port.AllowedVLANs...)
			if ep.Port.VLAN != nil {
				e.Port.VLAN = *ep.Port.VLAN
			}
			if ep.Port.NativeVLAN != nil {
				e.Port.NativeVLAN = *ep.Port.NativeVLAN
			}
		}
	}
	return e
}

func vmCloudNetwork(cfg *config.Config, name string, vm *config.VM) cloudinit.Network {
	var n cloudinit.Network
	for _, i := range vm.Interfaces {
		in := i.Name
		ci := cloudinit.Interface{Name: in, MAC: i.MAC, MTU: i.MTU, IPv4: vmIPv4(i.IPv4)}
		for _, v := range i.VLANs {
			id := v.ID
			ci.VLANs = append(ci.VLANs, cloudinit.VLAN{ID: id, MTU: v.MTU, IPv4: vmIPv4(v.IPv4)})
		}
		n.Interfaces = append(n.Interfaces, ci)
	}
	return n
}

func vmIPv4(v *config.VMIPv4) *cloudinit.IPv4 {
	if v == nil {
		return nil
	}
	out := &cloudinit.IPv4{DHCP: v.DHCP, Addresses: append([]string(nil), v.Addresses...), DNSServers: append([]string(nil), v.DNSServers...)}
	for _, r := range v.Routes {
		out.Routes = append(out.Routes, cloudinit.Route{To: r.To, Via: r.Via})
	}
	return out
}
func itoa(v int) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	b := [20]byte{}
	i := len(b)
	for v > 0 {
		i--
		b[i] = digits[v%10]
		v /= 10
	}
	return string(b[i:])
}
