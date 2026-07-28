package network

import (
	"context"
	"fmt"
	"strconv"
)

func (m *Manager) Verify(ctx context.Context, state *State) error {
	if state == nil {
		return fmt.Errorf("network: nil state")
	}
	for _, rule := range state.FirewallRules {
		handles, exists, err := m.firewallRuleHandles(ctx, rule)
		if err != nil {
			return err
		}
		if !exists || len(handles) != 1 {
			return fmt.Errorf("network: managed host forwarding rule %q is missing or duplicated", rule.Comment)
		}
	}
	for _, r := range state.Resources {
		switch r.Kind {
		case ResourceNamespace:
			out, err := m.run.Run(ctx, "ip", "netns", "list")
			if err != nil || !namespaceListed(string(out), r.Name) {
				return fmt.Errorf("network: managed namespace %s is missing", r.Name)
			}
			owned, err := m.namespaceOwned(ctx, r.Name, r.Alias)
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("network: namespace %s has wrong ownership marker", r.Name)
			}
		case ResourceNFTTable:
			owned, exists, err := m.nftOwned(ctx, r.Namespace, r.Name, r.Alias)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("network: managed nft table %s is missing", r.Name)
			}
			if !owned {
				return fmt.Errorf("network: nft table %s has wrong ownership marker", r.Name)
			}
		case ResourceProcess:
			// Helper-process identity is verified by its owning reconciler.
		default:
			owned, err := m.ownedLink(ctx, r.Namespace, r.Name, r.Alias)
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("network: managed interface %s is missing", r.Name)
			}
		}
	}
	return nil
}

func (m *Manager) Remove(ctx context.Context, state *State) error {
	if state == nil {
		return nil
	}
	// Validate every present resource before deleting any of them. Missing
	// resources are recoverable drift; wrong markers abort the whole cleanup.
	present := map[string]bool{}
	namespacePresent := map[string]bool{}
	firewallHandles := make([]uint64, len(state.FirewallRules))
	firewallPresent := make([]bool, len(state.FirewallRules))
	resourceKey := func(r Resource) string { return string(r.Kind) + "\x00" + r.Name }
	for i, rule := range state.FirewallRules {
		handles, exists, err := m.firewallRuleHandles(ctx, rule)
		if err != nil {
			return err
		}
		if !exists || len(handles) == 0 {
			continue
		}
		if len(handles) != 1 {
			return fmt.Errorf("network: refusing to remove duplicated host forwarding rule %q", rule.Comment)
		}
		firewallHandles[i], firewallPresent[i] = handles[0], true
	}
	for _, r := range state.Resources {
		switch r.Kind {
		case ResourceNamespace:
			out, err := m.run.Run(ctx, "ip", "netns", "list")
			if err != nil {
				return err
			}
			if !namespaceListed(string(out), r.Name) {
				continue
			}
			present[resourceKey(r)] = true
			namespacePresent[r.Name] = true
			owned, err := m.namespaceOwned(ctx, r.Name, r.Alias)
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("network: refusing to remove namespace %s with wrong ownership marker", r.Name)
			}
			pids, err := m.ns.PIDs(ctx, r.Name)
			if err != nil {
				return fmt.Errorf("network: list processes in namespace %s: %w", r.Name, err)
			}
			if len(pids) != 0 {
				return fmt.Errorf("network: refusing to remove namespace %s with unowned processes %v", r.Name, pids)
			}
		case ResourceNFTTable:
			if r.Namespace != "" && !namespacePresent[r.Namespace] {
				continue
			}
			owned, exists, err := m.nftOwned(ctx, r.Namespace, r.Name, r.Alias)
			if err != nil {
				return err
			}
			if exists && !owned {
				return fmt.Errorf("network: refusing to remove nft table %s with wrong ownership marker", r.Name)
			}
			present[resourceKey(r)] = exists
		case ResourceProcess:
		default:
			if r.Namespace != "" && !namespacePresent[r.Namespace] {
				continue
			}
			owned, err := m.ownedLink(ctx, r.Namespace, r.Name, r.Alias)
			if err != nil {
				return err
			}
			if !owned {
				// Distinguish absent from present-but-wrong: ownedLink already
				// returns an error for a present interface with a wrong alias.
				continue
			}
			present[resourceKey(r)] = true
		}
	}
	for i, rule := range state.FirewallRules {
		if !firewallPresent[i] {
			continue
		}
		if _, err := m.run.Run(ctx, "nft", "delete", "rule", rule.Family, rule.Table, rule.Chain, "handle", strconv.FormatUint(firewallHandles[i], 10)); err != nil && !commandReportsNotFound(err) {
			return fmt.Errorf("network: remove host forwarding rule %q: %w", rule.Comment, err)
		}
	}
	for _, ext := range state.External {
		hostDevice := externalForwardDevice(state.Topology, ext.External)
		for _, source := range ext.Sources {
			if _, err := m.run.Run(ctx, "ip", "route", "del", source, "via", ext.RouterAddress, "dev", hostDevice, "proto", "static"); err != nil && !commandReportsNotFound(err) {
				return err
			}
		}
	}
	for i := len(state.Resources) - 1; i >= 0; i-- {
		r := state.Resources[i]
		if !present[resourceKey(r)] {
			continue
		}
		var err error
		switch r.Kind {
		case ResourceNFTTable:
			_, err = m.runIn(ctx, r.Namespace, "nft", "delete", "table", "inet", r.Name)
		case ResourceNamespace:
			pids, listErr := m.ns.PIDs(ctx, r.Name)
			if listErr != nil {
				return fmt.Errorf("network: list processes in namespace %s: %w", r.Name, listErr)
			}
			if len(pids) != 0 {
				return fmt.Errorf("network: refusing to remove namespace %s with unowned processes %v", r.Name, pids)
			}
			_, err = m.run.Run(ctx, "ip", "netns", "del", r.Name)
		case ResourceLink, ResourceBridge, ResourceTAP:
			_, err = m.runIn(ctx, r.Namespace, "ip", "link", "del", "dev", r.Name)
		}
		if err != nil && !commandReportsNotFound(err) {
			return fmt.Errorf("network: remove %s %s: %w", r.Kind, r.Name, err)
		}
	}
	if state.ForwardingOwner {
		if err := m.releaseForwarding(ctx, state.SharedDir, state.Topology); err != nil {
			return err
		}
	}
	return nil
}
