package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/state"
)

func (a *App) reconcileNamespaceHelpers(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, ns *network.State) error {
	if m.Helpers == nil {
		m.Helpers = map[string]*state.Helper{}
	}
	desired := map[string]*state.Helper{}
	for _, routerName := range cfg.RouterNames() {
		r := cfg.Routers[routerName]
		namespace := ns.Namespaces[routerName]
		for _, ifaceName := range cfg.RouterInterfaceNames(routerName) {
			i := r.Interfaces[ifaceName]
			if i.IPv4 != nil && i.IPv4.DHCPServer != nil {
				addDNSMasqHelper(desired, layout, routerName, ifaceName, namespace)
			}
			for _, id := range cfg.RouterVLANIDs(routerName, ifaceName) {
				v := i.VLANs[id]
				if v.IPv4 != nil && v.IPv4.DHCPServer != nil {
					addDNSMasqHelper(desired, layout, routerName, ifaceName+"."+strconv.Itoa(id), namespace)
				}
			}
		}
	}
	for key, helper := range m.Helpers {
		if _, ok := desired[key]; ok {
			continue
		}
		stopErr := a.stopManagedHelper(helper)
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if stopErr != nil {
			return stopErr
		}
		if err := removeHelperFiles(helper, true); err != nil {
			return err
		}
		delete(m.Helpers, key)
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	for key, want := range desired {
		have := m.Helpers[key]
		if have != nil && have.Process != nil && a.Processes.Alive(have.Process.PID) {
			if err := verifyHelperIdentity(a.Processes, have.Process); err != nil {
				return err
			}
			continue
		}
		dhcp := findDHCP(cfg, want.Router, want.Interface)
		if dhcp == nil {
			return fmt.Errorf("missing DHCP configuration for %s", key)
		}
		// Write ownership before creating any helper artifact. A failed start is
		// therefore safe to retry and safe for down to clean.
		want.Process = nil
		m.Helpers[key] = want
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(want.ConfigPath), 0o700); err != nil {
			return err
		}
		if err := state.AtomicWrite(want.ConfigPath, []byte(renderDNSMasqConfig(want.Interface, dhcp)), 0o600); err != nil {
			return err
		}
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if err := ensureHelperFile(want.LeasePath, false); err != nil {
			return err
		}
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if err := ensureHelperFile(want.PIDPath, true); err != nil {
			return err
		}
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		marker := "--pid-file=" + want.PIDPath
		argv := []string{"ip", "netns", "exec", want.Namespace, "dnsmasq", "--keep-in-foreground", "--conf-file=" + want.ConfigPath, marker, "--dhcp-leasefile=" + want.LeasePath}
		started, err := a.StartProcess(argv, nil)
		if err != nil {
			return fmt.Errorf("start dnsmasq %s: %w", key, err)
		}
		defer started.DiscardStartupStderr()
		want.Process = &state.Process{PID: started.PID, StartedAt: a.Now().UTC(), Executable: "dnsmasq", IdentityArg: marker}
		m.Helpers[key] = want
		if err := state.Save(layout.ManifestPath, m); err != nil {
			stopErr := a.stopManagedHelper(want)
			if stopErr != nil {
				return fmt.Errorf("checkpoint dnsmasq %s: %v (rollback: %w)", key, err, stopErr)
			}
			return fmt.Errorf("checkpoint dnsmasq %s: %w", key, err)
		}
		if started.Cmd != nil {
			time.Sleep(50 * time.Millisecond)
			if !a.Processes.Alive(started.PID) {
				if stderr := started.StartupStderr(); stderr != "" {
					return fmt.Errorf("dnsmasq %s exited during startup: %s", key, stderr)
				}
				return fmt.Errorf("dnsmasq %s exited during startup", key)
			}
			if err := verifyHelperIdentity(a.Processes, want.Process); err != nil {
				return err
			}
			started.DiscardStartupStderr()
		}
	}
	return nil
}

func ensureHelperFile(path string, truncate bool) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("helper path is not a regular file: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func addDNSMasqHelper(out map[string]*state.Helper, layout state.Layout, router, iface, namespace string) {
	key := router + ":" + iface
	base := filepath.Join(layout.NodesDir, router, "dnsmasq-"+state.ShortID(iface, 10))
	out[key] = &state.Helper{Kind: "dnsmasq", Router: router, Interface: iface, Namespace: namespace, ConfigPath: base + ".conf", PIDPath: base + ".pid", LeasePath: base + ".leases"}
}

func findDHCP(cfg *config.Config, router, logical string) *config.DHCPServer {
	r := cfg.Routers[router]
	if i := r.Interfaces[logical]; i != nil && i.IPv4 != nil {
		return i.IPv4.DHCPServer
	}
	for name, iface := range r.Interfaces {
		for id, vlan := range iface.VLANs {
			if logical == name+"."+strconv.Itoa(id) && vlan.IPv4 != nil {
				return vlan.IPv4.DHCPServer
			}
		}
	}
	return nil
}

func renderDNSMasqConfig(iface string, d *config.DHCPServer) string {
	var b strings.Builder
	// Each DHCP-enabled router interface has its own supervised dnsmasq in the
	// same network namespace. Disable DNS service so those helpers do not all
	// contend for the namespace's port 53; DNS is supplied to clients via the
	// configured DHCP option instead.
	b.WriteString("port=0\nbind-interfaces\ninterface=" + iface + "\ndhcp-authoritative\n")
	b.WriteString("dhcp-range=" + d.RangeStart + "," + d.RangeEnd + "," + d.Lease + "\n")
	if len(d.DNSServers) > 0 {
		b.WriteString("dhcp-option=option:dns-server," + strings.Join(d.DNSServers, ",") + "\n")
	}
	b.WriteString("dhcp-option-force=option:domain-search,topolab\nlog-dhcp\n")
	return b.String()
}

func (a *App) stopNamespaceHelpers(m *state.Manifest) error {
	if m == nil {
		return nil
	}
	for _, h := range m.Helpers {
		if err := a.stopManagedHelper(h); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) stopManagedHelper(h *state.Helper) error {
	if h == nil || h.Process == nil || !a.Processes.Alive(h.Process.PID) {
		if h != nil {
			h.Process = nil
		}
		return nil
	}
	if err := verifyHelperIdentity(a.Processes, h.Process); err != nil {
		return err
	}
	if err := a.Processes.Signal(h.Process.PID, syscall.SIGTERM); err != nil && a.Processes.Alive(h.Process.PID) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.Processes.Alive(h.Process.PID) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if a.Processes.Alive(h.Process.PID) {
		if err := verifyHelperIdentity(a.Processes, h.Process); err != nil {
			return err
		}
		if err := a.Processes.Signal(h.Process.PID, syscall.SIGKILL); err != nil && a.Processes.Alive(h.Process.PID) {
			return err
		}
	}
	h.Process = nil
	_ = os.Remove(h.PIDPath)
	return nil
}

func verifyHelperIdentity(processes interface {
	Alive(int) bool
	CommandLine(int) ([]string, error)
}, p *state.Process) error {
	if p == nil || p.PID <= 0 {
		return errors.New("invalid helper process")
	}
	args, err := processes.CommandLine(p.PID)
	if err != nil {
		return err
	}
	for _, arg := range args {
		if arg == p.IdentityArg {
			return nil
		}
	}
	return fmt.Errorf("PID %d does not identify owned helper %s", p.PID, p.IdentityArg)
}

func removeHelperFiles(h *state.Helper, includeLease bool) error {
	if h == nil {
		return nil
	}
	paths := []string{h.ConfigPath, h.PIDPath}
	if includeLease {
		paths = append(paths, h.LeasePath)
	}
	for _, p := range paths {
		if p != "" {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
