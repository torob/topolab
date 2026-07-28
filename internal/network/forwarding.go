package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type forwardingState struct {
	Original string                       `json:"original"`
	Owners   []string                     `json:"owners"`
	Guards   map[string][]forwardingGuard `json:"guards,omitempty"`
}

type forwardingGuard struct {
	HostDevice      string   `json:"host_device"`
	EgressInterface string   `json:"egress_interface"`
	Sources         []string `json:"sources"`
}

const (
	forwardingGuardTable  = "topolab_forward_guard"
	forwardingGuardMarker = "topolab:shared:forwarding-guard"
)

func (m *Manager) acquireForwarding(ctx context.Context, sharedDir, owner string, externals []ResolvedExternal) (bool, error) {
	guarded := false
	err := m.withForwardingLock(sharedDir, func(path string) error {
		st, err := loadForwarding(path)
		if err != nil {
			return err
		}
		if len(st.Owners) == 0 {
			out, err := m.run.Run(ctx, "sysctl", "-n", "net.ipv4.ip_forward")
			if err != nil {
				return err
			}
			st.Original = strings.TrimSpace(string(out))
			if st.Original != "0" && st.Original != "1" {
				return fmt.Errorf("network: unexpected ip_forward value %q", st.Original)
			}
		}
		originalState := cloneForwardingState(st)
		if st.Guards == nil {
			st.Guards = map[string][]forwardingGuard{}
		}
		changed := !contains(st.Owners, owner)
		if changed {
			st.Owners = append(st.Owners, owner)
			sort.Strings(st.Owners)
		}
		guards := make([]forwardingGuard, 0, len(externals))
		for _, ext := range externals {
			guards = append(guards, forwardingGuard{HostDevice: externalForwardDevice(owner, ext.External), EgressInterface: ext.EgressInterface, Sources: append([]string(nil), ext.Sources...)})
		}
		st.Guards[owner] = guards
		guarded = st.Original == "0"
		if err := saveForwarding(path, st); err != nil {
			return err
		}
		if guarded {
			if err := m.applyForwardingGuard(ctx, sharedDir, st); err != nil {
				_ = saveForwarding(path, originalState)
				return err
			}
		}
		if _, err := m.run.Run(ctx, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1"); err != nil {
			if changed {
				if rollbackErr := saveForwarding(path, originalState); rollbackErr != nil {
					return fmt.Errorf("network: enable forwarding: %v (rollback ownership: %w)", err, rollbackErr)
				}
			}
			return err
		}
		return nil
	})
	return guarded, err
}

func (m *Manager) releaseForwarding(ctx context.Context, sharedDir, owner string) error {
	return m.withForwardingLock(sharedDir, func(path string) error {
		st, err := loadForwarding(path)
		if err != nil {
			return err
		}
		if !contains(st.Owners, owner) {
			return nil
		}
		owners := st.Owners[:0]
		for _, v := range st.Owners {
			if v != owner {
				owners = append(owners, v)
			}
		}
		st.Owners = owners
		delete(st.Guards, owner)
		if len(st.Owners) == 0 {
			if st.Original != "0" && st.Original != "1" {
				return errors.New("network: forwarding ownership has invalid original value")
			}
			if _, err := m.run.Run(ctx, "sysctl", "-q", "-w", "net.ipv4.ip_forward="+st.Original); err != nil {
				return err
			}
			if st.Original == "0" {
				if err := m.removeForwardingGuard(ctx); err != nil {
					return err
				}
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		if err := saveForwarding(path, st); err != nil {
			return err
		}
		if st.Original == "0" {
			return m.applyForwardingGuard(ctx, sharedDir, st)
		}
		return nil
	})
}

func cloneForwardingState(st forwardingState) forwardingState {
	out := forwardingState{Original: st.Original, Owners: append([]string(nil), st.Owners...), Guards: map[string][]forwardingGuard{}}
	for owner, guards := range st.Guards {
		for _, guard := range guards {
			copyGuard := guard
			copyGuard.Sources = append([]string(nil), guard.Sources...)
			out.Guards[owner] = append(out.Guards[owner], copyGuard)
		}
	}
	return out
}

func (m *Manager) applyForwardingGuard(ctx context.Context, sharedDir string, st forwardingState) error {
	owned, exists, err := m.nftOwned(ctx, "", forwardingGuardTable, forwardingGuardMarker)
	if err != nil {
		return err
	}
	if exists && !owned {
		return fmt.Errorf("network: shared forwarding guard has wrong ownership marker")
	}
	if err := os.MkdirAll(sharedDir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(sharedDir, ".forwarding-guard-*.nft")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(forwardingGuardRuleset(st, exists)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, "nft", "-f", path); err != nil {
		return fmt.Errorf("network: atomically install shared forwarding guard: %w", err)
	}
	return nil
}

func forwardingGuardRuleset(st forwardingState, replace bool) string {
	var b strings.Builder
	if replace {
		fmt.Fprintf(&b, "delete table inet %s\n", forwardingGuardTable)
	}
	fmt.Fprintf(&b, "add table inet %s { comment %s; }\n", forwardingGuardTable, nftString(forwardingGuardMarker))
	fmt.Fprintf(&b, "add chain inet %s forward { type filter hook forward priority -20; policy drop; }\n", forwardingGuardTable)
	for _, owner := range st.Owners {
		for _, guard := range st.Guards[owner] {
			for _, source := range guard.Sources {
				comment := ownerAlias(owner, "global-forward-guard", guard.HostDevice+":"+source)
				fmt.Fprintf(&b, "add rule inet %s forward iifname %s oifname %s ip saddr %s accept comment %s\n", forwardingGuardTable, nftString(guard.HostDevice), nftString(guard.EgressInterface), source, nftString(comment+":out"))
				fmt.Fprintf(&b, "add rule inet %s forward iifname %s oifname %s ip daddr %s ct state { established, related } accept comment %s\n", forwardingGuardTable, nftString(guard.EgressInterface), nftString(guard.HostDevice), source, nftString(comment+":in"))
			}
		}
	}
	return b.String()
}

func (m *Manager) removeForwardingGuard(ctx context.Context) error {
	owned, exists, err := m.nftOwned(ctx, "", forwardingGuardTable, forwardingGuardMarker)
	if err != nil || !exists {
		return err
	}
	if !owned {
		return fmt.Errorf("network: shared forwarding guard has wrong ownership marker")
	}
	_, err = m.run.Run(ctx, "nft", "delete", "table", "inet", forwardingGuardTable)
	return err
}

func (m *Manager) withForwardingLock(sharedDir string, fn func(string) error) error {
	if err := os.MkdirAll(sharedDir, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(sharedDir, "ipv4-forwarding.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn(filepath.Join(sharedDir, "ipv4-forwarding.json"))
}

func loadForwarding(path string) (forwardingState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return forwardingState{}, nil
	}
	if err != nil {
		return forwardingState{}, err
	}
	var st forwardingState
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("network: decode forwarding ownership: %w", err)
	}
	return st, nil
}

func saveForwarding(path string, st forwardingState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".forwarding-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
