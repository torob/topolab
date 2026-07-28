package network

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
)

type hostRoute struct {
	Prefix                netip.Prefix
	Device, Via, Protocol string
}

func (m *Manager) hostRoutes(ctx context.Context, owned []OwnedRoute) ([]hostRoute, error) {
	out, err := m.run.Run(ctx, "ip", "-j", "-4", "route", "show", "table", "all", "type", "unicast")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Dst      string `json:"dst"`
		Dev      string `json:"dev"`
		Gateway  string `json:"gateway"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	var result []hostRoute
	for _, row := range rows {
		if row.Dst == "" || row.Dst == "default" {
			continue
		}
		p, err := netip.ParsePrefix(row.Dst)
		if err == nil && p.Addr().Is4() {
			route := hostRoute{Prefix: p.Masked(), Device: row.Dev, Via: row.Gateway, Protocol: row.Protocol}
			if candidate := exactOwnedRoute(route, owned); candidate != nil {
				if candidate.Marker == "" {
					return nil, fmt.Errorf("network: owned route %s on %s has no ownership marker", route.Prefix, route.Device)
				}
				isOwned, ownErr := m.ownedLink(ctx, "", route.Device, candidate.Marker)
				if ownErr != nil {
					return nil, ownErr
				}
				if isOwned {
					continue
				}
			}
			result = append(result, route)
		}
	}
	return result, nil
}

func exactOwnedRoute(route hostRoute, owned []OwnedRoute) *OwnedRoute {
	for i := range owned {
		candidate := &owned[i]
		prefix, err := netip.ParsePrefix(candidate.Prefix)
		if err != nil {
			continue
		}
		if prefix.Masked() != route.Prefix || candidate.Device != route.Device || candidate.Via != route.Via {
			continue
		}
		if candidate.Protocol != "" && candidate.Protocol != route.Protocol {
			continue
		}
		return candidate
	}
	return nil
}

func (m *Manager) routeEgress(ctx context.Context) (string, string, error) {
	out, err := m.run.Run(ctx, "ip", "-j", "-4", "route", "get", "1.1.1.1")
	if err != nil {
		return "", "", err
	}
	var rows []struct{ Dev, PrefSrc, Src string }
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", "", err
	}
	if len(rows) != 1 || rows[0].Dev == "" {
		return "", "", fmt.Errorf("default route lookup returned no device")
	}
	src := rows[0].PrefSrc
	if src == "" {
		src = rows[0].Src
	}
	return rows[0].Dev, src, nil
}

func (m *Manager) interfaceSource(ctx context.Context, device string) (string, error) {
	out, err := m.run.Run(ctx, "ip", "-j", "-4", "addr", "show", "dev", device)
	if err != nil {
		return "", err
	}
	var rows []struct {
		AddrInfo []struct{ Local, Scope string } `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", err
	}
	for _, row := range rows {
		for _, a := range row.AddrInfo {
			if a.Local != "" && a.Scope == "global" {
				return a.Local, nil
			}
		}
	}
	return "", fmt.Errorf("interface %s has no usable global IPv4 address", device)
}

func (m *Manager) verifyInterfaceAddress(ctx context.Context, device, address string) error {
	ip, err := netip.ParseAddr(address)
	if err != nil || !ip.Is4() {
		return fmt.Errorf("network: invalid external source IP %q", address)
	}
	out, err := m.run.Run(ctx, "ip", "-j", "-4", "addr", "show", "dev", device)
	if err != nil {
		return err
	}
	var rows []struct {
		AddrInfo []struct{ Local string } `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return err
	}
	for _, row := range rows {
		for _, a := range row.AddrInfo {
			if a.Local == address {
				return nil
			}
		}
	}
	return fmt.Errorf("network: source IP %s is not assigned to %s", address, device)
}

func allocateTransit(topology, router, iface string, used []netip.Prefix) (netip.Prefix, error) {
	const count = uint32(1 << 15) // /15 contains 32768 /30s
	sum := sha256.Sum256([]byte("topolab/transit/v1\x00" + topology + "\x00" + router + "\x00" + iface))
	start := binary.BigEndian.Uint32(sum[:4]) % count
	base := binary.BigEndian.Uint32([]byte{198, 18, 0, 0})
	for probe := uint32(0); probe < count; probe++ {
		n := (start + probe) % count
		v := base + n*4
		addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		candidate := netip.PrefixFrom(addr, 30)
		overlap := false
		for _, p := range used {
			if prefixesOverlap(candidate, p) {
				overlap = true
				break
			}
		}
		if !overlap {
			return candidate, nil
		}
	}
	return netip.Prefix{}, errorsNew("network: external transit pool exhausted")
}

func prefixesOverlap(a, b netip.Prefix) bool {
	return a.Contains(b.Masked().Addr()) || b.Contains(a.Masked().Addr())
}

// Avoid importing errors merely for one static allocation failure.
func errorsNew(s string) error { return fmt.Errorf("%s", s) }
