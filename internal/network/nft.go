package network

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func (m *Manager) applyHostNFT(ctx context.Context, state *State) error {
	if err := m.applyManagementHostNFT(ctx, state); err != nil {
		return err
	}
	if len(state.External) == 0 {
		return nil
	}
	table := nftTableName(state.Topology)
	marker := ownerAlias(state.Topology, "nft", "root-external")
	state.RootNFTTable = table
	state.Resources = append(state.Resources, Resource{Kind: ResourceNFTTable, Name: table, Alias: marker})
	// Apply is used for a datapath generation rebuild. Replacing only a table
	// bearing our marker keeps retries deterministic and avoids duplicate rules.
	if owned, exists, err := m.nftOwned(ctx, "", table, marker); err != nil {
		return err
	} else if exists {
		if !owned {
			return fmt.Errorf("network: nft table %s has wrong ownership marker", table)
		}
		if _, err := m.run.Run(ctx, "nft", "delete", "table", "inet", table); err != nil {
			return err
		}
	}
	if _, err := m.run.Run(ctx, "nft", "add", "table", "inet", table, "{", "comment", nftString(marker), ";", "}"); err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, "nft", "add", "chain", "inet", table, "forward", "{", "type", "filter", "hook", "forward", "priority", "-10", ";", "policy", "accept", ";", "}"); err != nil {
		return err
	}
	if len(state.External) > 0 {
		if _, err := m.run.Run(ctx, "nft", "add", "chain", "inet", table, "postrouting", "{", "type", "nat", "hook", "postrouting", "priority", "srcnat", ";", "policy", "accept", ";", "}"); err != nil {
			return err
		}
	}
	for _, ext := range state.External {
		hostDevice := externalForwardDevice(state.Topology, ext.External)
		commentBase := ext.Router + ":" + ext.Interface
		for _, source := range ext.Sources {
			if _, err := m.run.Run(ctx, "nft", "add", "rule", "inet", table, "forward", "iifname", hostDevice, "oifname", ext.EgressInterface, "ip", "saddr", source, "ct", "state", "new,established,related", "counter", "accept", "comment", nftString(ownerAlias(state.Topology, "external-forward-out", commentBase+":"+source))); err != nil {
				return err
			}
			if _, err := m.run.Run(ctx, "nft", "add", "rule", "inet", table, "forward", "iifname", ext.EgressInterface, "oifname", hostDevice, "ip", "daddr", source, "ct", "state", "established,related", "counter", "accept", "comment", nftString(ownerAlias(state.Topology, "external-forward-return", commentBase+":"+source))); err != nil {
				return err
			}
		}
		// Keep the table's policy non-invasive for unrelated host forwarding,
		// then close both directions of this topology-owned interface pair.
		if _, err := m.run.Run(ctx, "nft", "add", "rule", "inet", table, "forward", "iifname", hostDevice, "oifname", ext.EgressInterface, "counter", "drop", "comment", nftString(ownerAlias(state.Topology, "external-forward-drop", commentBase+":out"))); err != nil {
			return err
		}
		if _, err := m.run.Run(ctx, "nft", "add", "rule", "inet", table, "forward", "iifname", ext.EgressInterface, "oifname", hostDevice, "counter", "drop", "comment", nftString(ownerAlias(state.Topology, "external-forward-drop", commentBase+":in"))); err != nil {
			return err
		}
		for _, source := range ext.Sources {
			args := []string{"add", "rule", "inet", table, "postrouting", "ip", "saddr", source, "oifname", ext.EgressInterface, "counter"}
			if ext.Masquerade {
				args = append(args, "masquerade")
			} else {
				args = append(args, "snat", "to", ext.SourceIP)
			}
			args = append(args, "comment", nftString(ownerAlias(state.Topology, "external-nat", ext.Router+":"+ext.Interface+":"+source)))
			if _, err := m.run.Run(ctx, "nft", args...); err != nil {
				return err
			}
		}
	}
	return m.applyFirewallCompatibility(ctx, state)
}

func (m *Manager) applyManagementHostNFT(ctx context.Context, state *State) error {
	ns := state.ManagementHostNamespace
	table := DeviceName("tl", state.Topology, "management-host-nft")
	marker := ownerAlias(state.Topology, "nft", "management-host")
	state.ManagementHostNFTTable = table
	state.Resources = append(state.Resources, Resource{Kind: ResourceNFTTable, Name: table, Namespace: ns, Alias: marker})
	if owned, exists, err := m.nftOwned(ctx, ns, table, marker); err != nil {
		return err
	} else if exists {
		if !owned {
			return fmt.Errorf("network: nft table %s in %s has wrong ownership marker", table, ns)
		}
		if _, err := m.runIn(ctx, ns, "nft", "delete", "table", "inet", table); err != nil {
			return err
		}
	}
	if _, err := m.runIn(ctx, ns, "nft", "add", "table", "inet", table, "{", "comment", nftString(marker), ";", "}"); err != nil {
		return err
	}
	if _, err := m.runIn(ctx, ns, "nft", "add", "chain", "inet", table, "forward", "{", "type", "filter", "hook", "forward", "priority", "-10", ";", "policy", "drop", ";", "}"); err != nil {
		return err
	}
	for _, iface := range sortedManagementHostInterfaces(state.ManagementHostInterfaces) {
		comment := ownerAlias(state.Topology, "isolation", iface.LogicalName)
		if _, err := m.runIn(ctx, ns, "nft", "add", "rule", "inet", table, "forward", "iifname", iface.DeviceName, "counter", "drop", "comment", nftString(comment)); err != nil {
			return err
		}
		if _, err := m.runIn(ctx, ns, "nft", "add", "rule", "inet", table, "forward", "oifname", iface.DeviceName, "counter", "drop", "comment", nftString(comment)); err != nil {
			return err
		}
	}
	return nil
}

type nftForwardHook struct{ Family, Table, Chain string }

func (m *Manager) applyFirewallCompatibility(ctx context.Context, state *State) error {
	if len(state.External) == 0 {
		return nil
	}
	hooks, err := m.hostForwardHooks(ctx, state.RootNFTTable)
	if err != nil {
		return err
	}
	for _, hook := range hooks {
		for _, ext := range state.External {
			hostDevice := externalForwardDevice(state.Topology, ext.External)
			base := ext.Router + ":" + ext.Interface
			for _, source := range ext.Sources {
				rules := []struct {
					comment string
					args    []string
				}{
					{ownerAlias(state.Topology, "host-forward-compat-out", base+":"+source), []string{"iifname", hostDevice, "oifname", ext.EgressInterface, "ip", "saddr", source, "ct", "state", "new,established,related", "counter", "accept"}},
					{ownerAlias(state.Topology, "host-forward-compat-return", base+":"+source), []string{"iifname", ext.EgressInterface, "oifname", hostDevice, "ip", "daddr", source, "ct", "state", "established,related", "counter", "accept"}},
				}
				for _, rule := range rules {
					record := FirewallRule{Family: hook.Family, Table: hook.Table, Chain: hook.Chain, Comment: rule.comment}
					state.FirewallRules = append(state.FirewallRules, record)
					handles, exists, err := m.firewallRuleHandles(ctx, record)
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("network: host forward chain %s %s %s disappeared", hook.Family, hook.Table, hook.Chain)
					}
					if len(handles) > 1 {
						return fmt.Errorf("network: duplicate host forwarding ownership marker %q", record.Comment)
					}
					if len(handles) == 1 {
						continue
					}
					args := []string{"insert", "rule", hook.Family, hook.Table, hook.Chain}
					args = append(args, rule.args...)
					args = append(args, "comment", nftString(rule.comment))
					if _, err := m.run.Run(ctx, "nft", args...); err != nil {
						return fmt.Errorf("network: install host forwarding compatibility rule in %s %s %s: %w", hook.Family, hook.Table, hook.Chain, err)
					}
				}
			}
		}
	}
	return nil
}

func (m *Manager) hostForwardHooks(ctx context.Context, excludeTable string) ([]nftForwardHook, error) {
	out, err := m.run.Run(ctx, "nft", "-j", "list", "ruleset")
	if err != nil {
		return nil, fmt.Errorf("network: inspect host forward hooks: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	var doc struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("network: decode host forward hooks: %w", err)
	}
	var hooks []nftForwardHook
	for _, object := range doc.NFTables {
		raw, ok := object["chain"]
		if !ok {
			continue
		}
		var chain struct {
			Family, Table, Name, Hook string
		}
		if err := json.Unmarshal(raw, &chain); err != nil {
			return nil, fmt.Errorf("network: decode host forward chain: %w", err)
		}
		if chain.Hook != "forward" || (chain.Family != "ip" && chain.Family != "inet") || chain.Table == excludeTable || chain.Table == forwardingGuardTable {
			continue
		}
		hooks = append(hooks, nftForwardHook{Family: chain.Family, Table: chain.Table, Chain: chain.Name})
	}
	sort.Slice(hooks, func(i, j int) bool {
		a, b := hooks[i], hooks[j]
		return a.Family+"\x00"+a.Table+"\x00"+a.Chain < b.Family+"\x00"+b.Table+"\x00"+b.Chain
	})
	return hooks, nil
}

func (m *Manager) firewallRuleHandles(ctx context.Context, rule FirewallRule) ([]uint64, bool, error) {
	out, err := m.run.Run(ctx, "nft", "-j", "list", "chain", rule.Family, rule.Table, rule.Chain)
	if err != nil {
		if commandReportsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("network: inspect host forwarding rule %q: %w", rule.Comment, err)
	}
	var doc struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, true, fmt.Errorf("network: decode host forwarding rule %q: %w", rule.Comment, err)
	}
	var handles []uint64
	for _, object := range doc.NFTables {
		raw, ok := object["rule"]
		if !ok {
			continue
		}
		var value struct {
			Family, Table, Chain, Comment string
			Handle                        uint64
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, true, fmt.Errorf("network: decode host forwarding rule identity: %w", err)
		}
		if value.Family == rule.Family && value.Table == rule.Table && value.Chain == rule.Chain && value.Comment == rule.Comment {
			handles = append(handles, value.Handle)
		}
	}
	return handles, true, nil
}

func externalForwardDevice(topology string, ext External) string {
	return DeviceName("tlh", topology, ext.Router, ext.Interface, "external")
}

func (m *Manager) nftOwned(ctx context.Context, ns, table, marker string) (owned, exists bool, err error) {
	return m.nftOwnedFamily(ctx, ns, "inet", table, marker)
}

func (m *Manager) nftOwnedFamily(ctx context.Context, ns, family, table, marker string) (owned, exists bool, err error) {
	out, runErr := m.runIn(ctx, ns, "nft", "-j", "list", "table", family, table)
	if runErr != nil {
		if commandReportsNotFound(runErr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("network: inspect nft table %s: %w", table, runErr)
	}
	var doc struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return false, true, fmt.Errorf("network: decode nft table %s: %w", table, err)
	}
	for _, object := range doc.NFTables {
		raw, ok := object["table"]
		if !ok {
			continue
		}
		var value struct {
			Family, Name, Comment string
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return false, true, fmt.Errorf("network: decode nft table identity %s: %w", table, err)
		}
		if value.Family == family && value.Name == table {
			return value.Comment == marker, true, nil
		}
	}
	return false, true, fmt.Errorf("network: nft response omitted table %s %s", family, table)
}

func (m *Manager) configureNamespaceNAT(ctx context.Context, topology string, router Router, ns string) error {
	if len(router.NAT) == 0 {
		return nil
	}
	table := DeviceName("tl", topology, router.Name, "nat")
	marker := ownerAlias(topology, "nft", "router:"+router.Name)
	if owned, exists, err := m.nftOwnedFamily(ctx, ns, "ip", table, marker); err != nil {
		return err
	} else if exists {
		if !owned {
			return fmt.Errorf("network: router nft table %s has wrong ownership marker", table)
		}
		if _, err := m.runIn(ctx, ns, "nft", "delete", "table", "ip", table); err != nil {
			return err
		}
	}
	if _, err := m.runIn(ctx, ns, "nft", "add", "table", "ip", table, "{", "comment", nftString(marker), ";", "}"); err != nil {
		return err
	}
	if _, err := m.runIn(ctx, ns, "nft", "add", "chain", "ip", table, "postrouting", "{", "type", "nat", "hook", "postrouting", "priority", "srcnat", ";", "}"); err != nil {
		return err
	}
	for _, nat := range router.NAT {
		for _, source := range nat.Sources {
			if _, err := m.runIn(ctx, ns, "nft", "add", "rule", "ip", table, "postrouting", "ip", "saddr", source, "oifname", nat.Interface, "counter", "masquerade", "comment", nftString(ownerAlias(topology, "router-nat", router.Name+":"+nat.Interface+":"+source))); err != nil {
				return err
			}
		}
	}
	return nil
}

// configureDNSMasq performs a deterministic configuration check. Process
// supervision belongs to the reconciler because it owns PID paths and command
// line identity verification; this package supplies the exact arguments.
func (m *Manager) configureDNSMasq(ctx context.Context, topology string, router Router, ns string) error {
	for index, dhcp := range router.DHCP {
		args := DNSMasqArgs(topology, router.Name, index, dhcp)
		// --test validates availability and syntax without leaving an
		// unsupervised daemon if later Apply stages fail.
		args = append(args, "--test")
		if _, err := m.run.Run(ctx, "ip", append([]string{"netns", "exec", ns, "dnsmasq"}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

// DNSMasqArgs is exported for the process reconciler that starts dnsmasq in a
// namespace using its own verified PID and lease-file paths.
func DNSMasqArgs(topology, router string, index int, dhcp DHCPServer) []string {
	args := []string{"--conf-file=", "--bind-interfaces", "--interface=" + dhcp.Interface, "--dhcp-range=" + dhcp.RangeStart + "," + dhcp.RangeEnd + "," + dhcp.Lease, "--dhcp-authoritative"}
	if len(dhcp.DNSServers) > 0 {
		args = append(args, "--dhcp-option=option:dns-server,"+strings.Join(dhcp.DNSServers, ","))
	}
	args = append(args, "--dhcp-option-force=option:domain-search,topolab", "--log-facility=-", "--log-dhcp", "--tag-if=set:topolab,tag:!topolab-"+strconv.Itoa(index))
	return args
}

func sortedManagementHostInterfaces(values map[string]ManagementHostInterface) []ManagementHostInterface {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slicesSortStrings(keys)
	result := make([]ManagementHostInterface, 0, len(keys))
	for _, k := range keys {
		result = append(result, values[k])
	}
	return result
}

func nftString(value string) string { return strconv.Quote(value) }

func slicesSortStrings(v []string) { // keeps this file's imports compact
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
