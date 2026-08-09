package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/observability"
	"github.com/torob/topolab/internal/state"
)

func (a *App) runRead(ctx context.Context, command Command, layout state.Layout, m *state.Manifest) error {
	switch command.Name {
	case "ps":
		return a.ps(ctx, command.Topology, m, command.Args)
	case "logs":
		return a.logs(ctx, command.Topology, m, command.Args)
	case "capture":
		return a.capture(ctx, command.Topology, layout, m, command.Args)
	case "counters":
		return a.counters(ctx, command.Topology, m, command.Args)
	}
	return errors.New("unknown read command")
}

func (a *App) ps(ctx context.Context, cfg *config.Config, m *state.Manifest, args []string) error {
	format := "table"
	formatSet := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--format" || i+1 >= len(args) {
			return fmt.Errorf("ps accepts only --format table|json")
		}
		i++
		if formatSet || (args[i] != "table" && args[i] != "json") {
			return errors.New("ps --format must be table or json and may be specified only once")
		}
		format = args[i]
		formatSet = true
	}
	if format == "json" {
		return a.psJSON(ctx, cfg, m)
	}
	return a.psTable(ctx, cfg, m)
}

func (a *App) psTable(ctx context.Context, cfg *config.Config, m *state.Manifest) error {
	if m == nil {
		fmt.Fprintln(a.Stdout, "NAME\tTYPE\tSTATE\tPID\tDETAILS")
		return nil
	}
	netState := oldNetwork(m)
	networkStatus := "absent"
	if netState != nil {
		if err := a.Network.Verify(ctx, netState); err == nil {
			networkStatus = "ready"
		} else {
			networkStatus = "drift: " + err.Error()
		}
	}
	fmt.Fprintln(a.Stdout, "NAME\tTYPE\tSTATE\tPID\tDETAILS")
	for _, name := range cfg.VMNames() {
		n := m.Nodes[name]
		status, pid, details := "stopped", 0, "network="+networkStatus
		if n != nil && n.PID > 0 && a.Processes.Alive(n.PID) {
			status, pid = "running", n.PID
			if n.NetworkNamespace == "" {
				status = "drift: QEMU namespace is unrecorded"
			} else if same, err := a.Namespaces.SameProcess(n.PID, n.NetworkNamespace); err != nil || !same {
				status = "drift: QEMU outside fabric namespace"
			}
			client := a.OpenQMP(n.QMPPath)
			if qstatus, err := client.QueryStatus(ctx); err == nil {
				status = qstatus
			}
			if n.BalloonTargetMB != nil {
				details += fmt.Sprintf(" balloon_target_mb=%d", *n.BalloonTargetMB)
				if current, err := client.QueryBalloon(ctx); err == nil {
					details += fmt.Sprintf(" balloon_observed_mb=%d", current>>20)
				} else {
					details += " balloon_observed=pending"
				}
			}
			_ = client.Close()
		}
		if n != nil {
			var paths []string
			for iface, tap := range n.TAPs {
				paths = append(paths, iface+"="+tap.Datapath)
			}
			slices.Sort(paths)
			if len(paths) > 0 {
				details += " datapath=" + strings.Join(paths, ",")
			}
			if vm, ok := cfg.LookupVM(name); ok && vm.GuestAgent {
				probeCtx, cancel := context.WithTimeout(ctx, time.Second)
				available := a.GuestAgentProbe(probeCtx, n.GuestAgentPath)
				cancel()
				if available {
					details += " guest_agent=available"
				} else {
					details += " guest_agent=unavailable"
				}
			}
		}
		fmt.Fprintf(a.Stdout, "%s\tvm\t%s\t%d\t%s\n", name, status, pid, details)
	}
	for _, name := range cfg.RouterNames() {
		status := networkStatus
		details := ""
		if netState != nil {
			for _, e := range netState.External {
				if e.Router == name {
					details = fmt.Sprintf("external=%s egress=%s transit=%s", e.Interface, e.EgressInterface, e.TransitPrefix)
				}
			}
		}
		fmt.Fprintf(a.Stdout, "%s\trouter\t%s\t-\t%s\n", name, status, details)
	}
	for _, name := range cfg.SwitchNames() {
		fmt.Fprintf(a.Stdout, "%s\tswitch\t%s\t-\t\n", name, networkStatus)
	}
	if netState != nil {
		fmt.Fprintf(a.Stdout, "fabric-namespace\tnamespace\t%s\t-\tname=%s\n", networkStatus, netState.FabricNamespace)
		fmt.Fprintf(a.Stdout, "management-host-namespace\tnamespace\t%s\t-\tname=%s\n", networkStatus, netState.ManagementHostNamespace)
		for _, name := range cfg.ManagementHostInterfaceNames() {
			h, ok := netState.ManagementHostInterfaces[name]
			if !ok {
				fmt.Fprintf(a.Stdout, "management_host:%s\tmanagement-host-interface\tabsent\t-\t\n", name)
				continue
			}
			fmt.Fprintf(a.Stdout, "management_host:%s\tmanagement-host-interface\t%s\t-\tdevice=%s mtu=%d addresses=%s link=%s isolation=%t namespace=%s\n", name, networkStatus, h.DeviceName, h.MTU, strings.Join(h.Addresses, ","), h.Link, netState.ManagementHostNFTTable != "", netState.ManagementHostNamespace)
		}
	}
	return nil
}

type psStatus struct {
	State      string `json:"state"`
	DriftError string `json:"drift_error,omitempty"`
}

type psReport struct {
	SchemaVersion            int                         `json:"schema_version"`
	Topology                 psTopology                  `json:"topology"`
	Network                  psStatus                    `json:"network"`
	Nodes                    []psNode                    `json:"nodes"`
	Namespaces               []psNamespace               `json:"namespaces"`
	ManagementHostInterfaces []psManagementHostInterface `json:"management_host_interfaces"`
}

type psTopology struct {
	Name string `json:"name"`
	psStatus
}

type psNode struct {
	Name               string                `json:"name"`
	Type               string                `json:"type"`
	State              string                `json:"state"`
	DriftError         string                `json:"drift_error,omitempty"`
	QEMU               *psQEMU               `json:"qemu,omitempty"`
	Datapaths          []psDatapath          `json:"datapaths,omitempty"`
	Balloon            *psBalloon            `json:"balloon,omitempty"`
	GuestAgent         *psGuestAgent         `json:"guest_agent,omitempty"`
	ExternalInterfaces []psExternalInterface `json:"external_interfaces,omitempty"`
}

type psQEMU struct {
	State      string `json:"state"`
	PID        int    `json:"pid"`
	Namespace  string `json:"namespace,omitempty"`
	DriftError string `json:"drift_error,omitempty"`
}

type psDatapath struct {
	Interface string `json:"interface"`
	Backend   string `json:"backend"`
}

type psBalloon struct {
	TargetMB     uint64  `json:"target_mb"`
	ObservedMB   *uint64 `json:"observed_mb,omitempty"`
	ObserveError string  `json:"observe_error,omitempty"`
}

type psGuestAgent struct {
	Available bool `json:"available"`
}

type psExternalInterface struct {
	Interface       string   `json:"interface"`
	EgressInterface string   `json:"egress_interface"`
	SourceIP        string   `json:"source_ip"`
	Sources         []string `json:"sources"`
	TransitPrefix   string   `json:"transit_prefix"`
	RouterAddress   string   `json:"router_address"`
	HostAddress     string   `json:"host_address"`
	Masquerade      bool     `json:"masquerade"`
}

type psNamespace struct {
	Role       string `json:"role"`
	Node       string `json:"node,omitempty"`
	Name       string `json:"name"`
	State      string `json:"state"`
	DriftError string `json:"drift_error,omitempty"`
}

type psManagementHostInterface struct {
	Name       string   `json:"name"`
	State      string   `json:"state"`
	DriftError string   `json:"drift_error,omitempty"`
	Device     string   `json:"device,omitempty"`
	MTU        int      `json:"mtu,omitempty"`
	Addresses  []string `json:"addresses,omitempty"`
	Link       string   `json:"link,omitempty"`
	Namespace  string   `json:"namespace,omitempty"`
	Isolated   bool     `json:"isolated"`
}

func (a *App) psJSON(ctx context.Context, cfg *config.Config, m *state.Manifest) error {
	report := psReport{SchemaVersion: 1, Topology: psTopology{Name: cfg.Name, psStatus: psStatus{State: "absent"}}, Network: psStatus{State: "absent"}, Nodes: []psNode{}, Namespaces: []psNamespace{}, ManagementHostInterfaces: []psManagementHostInterface{}}
	var netState *network.State
	if m != nil {
		report.Topology.State = "active"
		netState = oldNetwork(m)
		if netState != nil {
			report.Network.State = "ready"
			if err := a.Network.Verify(ctx, netState); err != nil {
				report.Network.State = "drift"
				report.Network.DriftError = err.Error()
				report.Topology.State = "drift"
				report.Topology.DriftError = err.Error()
			}
		}
	}
	for _, name := range cfg.VMNames() {
		node := psNode{Name: name, Type: "vm", State: "absent", QEMU: &psQEMU{State: "absent"}}
		var recorded *state.Node
		if m != nil {
			recorded = m.Nodes[name]
		}
		if recorded != nil {
			node.State, node.QEMU.State = "stopped", "stopped"
			node.QEMU.PID, node.QEMU.Namespace = recorded.PID, recorded.NetworkNamespace
			if recorded.PID > 0 && a.Processes.Alive(recorded.PID) {
				node.State, node.QEMU.State = "running", "running"
				if recorded.NetworkNamespace == "" {
					node.State, node.QEMU.State = "drift", "drift"
					node.DriftError, node.QEMU.DriftError = "QEMU namespace is unrecorded", "QEMU namespace is unrecorded"
				} else if same, err := a.Namespaces.SameProcess(recorded.PID, recorded.NetworkNamespace); err != nil || !same {
					node.State, node.QEMU.State = "drift", "drift"
					if err != nil {
						node.QEMU.DriftError = err.Error()
					} else {
						node.QEMU.DriftError = "QEMU is outside its recorded namespace"
					}
					node.DriftError = node.QEMU.DriftError
				}
				client := a.OpenQMP(recorded.QMPPath)
				if status, err := client.QueryStatus(ctx); err == nil && node.QEMU.State != "drift" {
					node.State, node.QEMU.State = status, status
				} else if err != nil && node.QEMU.DriftError == "" {
					node.State, node.QEMU.State = "drift", "drift"
					node.DriftError, node.QEMU.DriftError = err.Error(), err.Error()
				}
				if recorded.BalloonTargetMB != nil {
					node.Balloon = &psBalloon{TargetMB: *recorded.BalloonTargetMB}
					if current, err := client.QueryBalloon(ctx); err == nil {
						observed := current >> 20
						node.Balloon.ObservedMB = &observed
					} else {
						node.Balloon.ObserveError = err.Error()
					}
				}
				_ = client.Close()
			} else if recorded.BalloonTargetMB != nil {
				node.Balloon = &psBalloon{TargetMB: *recorded.BalloonTargetMB}
			}
			for iface, tap := range recorded.TAPs {
				node.Datapaths = append(node.Datapaths, psDatapath{Interface: iface, Backend: tap.Datapath})
			}
			slices.SortFunc(node.Datapaths, func(a, b psDatapath) int { return strings.Compare(a.Interface, b.Interface) })
		}
		if vm, ok := cfg.LookupVM(name); ok && vm.GuestAgent {
			node.GuestAgent = &psGuestAgent{}
			if recorded != nil {
				probeCtx, cancel := context.WithTimeout(ctx, time.Second)
				node.GuestAgent.Available = a.GuestAgentProbe(probeCtx, recorded.GuestAgentPath)
				cancel()
			}
		}
		report.Nodes = append(report.Nodes, node)
	}
	for _, name := range cfg.RouterNames() {
		node := psNode{Name: name, Type: "router", State: report.Network.State}
		if report.Network.DriftError != "" {
			node.DriftError = report.Network.DriftError
		}
		if netState != nil {
			for _, ext := range netState.External {
				if ext.Router == name {
					node.ExternalInterfaces = append(node.ExternalInterfaces, psExternalInterface{Interface: ext.Interface, EgressInterface: ext.EgressInterface, SourceIP: ext.SourceIP, Sources: append([]string(nil), ext.Sources...), TransitPrefix: ext.TransitPrefix, RouterAddress: ext.RouterAddress, HostAddress: ext.HostAddress, Masquerade: ext.Masquerade})
				}
			}
			slices.SortFunc(node.ExternalInterfaces, func(a, b psExternalInterface) int { return strings.Compare(a.Interface, b.Interface) })
		}
		report.Nodes = append(report.Nodes, node)
	}
	for _, name := range cfg.SwitchNames() {
		report.Nodes = append(report.Nodes, psNode{Name: name, Type: "switch", State: report.Network.State, DriftError: report.Network.DriftError})
	}
	if netState != nil {
		for _, item := range []struct{ role, node, name string }{{"fabric", "", netState.FabricNamespace}, {"management-host", "", netState.ManagementHostNamespace}} {
			report.Namespaces = append(report.Namespaces, psNamespace{Role: item.role, Node: item.node, Name: item.name, State: report.Network.State, DriftError: report.Network.DriftError})
		}
		for _, name := range cfg.RouterNames() {
			report.Namespaces = append(report.Namespaces, psNamespace{Role: "router", Node: name, Name: netState.Namespaces[name], State: report.Network.State, DriftError: report.Network.DriftError})
		}
	}
	for _, name := range cfg.ManagementHostInterfaceNames() {
		item := psManagementHostInterface{Name: name, State: "absent"}
		if netState != nil {
			if iface, ok := netState.ManagementHostInterfaces[name]; ok {
				item.State, item.DriftError = report.Network.State, report.Network.DriftError
				item.Device, item.MTU, item.Addresses, item.Link = iface.DeviceName, iface.MTU, append([]string(nil), iface.Addresses...), iface.Link
				item.Namespace, item.Isolated = netState.ManagementHostNamespace, netState.ManagementHostNFTTable != ""
			}
		}
		report.ManagementHostInterfaces = append(report.ManagementHostInterfaces, item)
	}
	if report.Topology.State != "drift" {
		for _, node := range report.Nodes {
			if node.State == "drift" {
				report.Topology.State = "drift"
				report.Topology.DriftError = fmt.Sprintf("node %s: %s", node.Name, node.DriftError)
				break
			}
		}
	}
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

type logsOptions struct {
	follow, timestamps bool
	tail               int
	nodes              []string
}

func parseLogs(args []string) (logsOptions, error) {
	o := logsOptions{tail: -1}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--follow", "-f":
			o.follow = true
		case "--timestamps":
			o.timestamps = true
		case "--tail":
			if i+1 >= len(args) {
				return o, errors.New("--tail requires N or all")
			}
			i++
			if args[i] == "all" {
				o.tail = -1
			} else {
				n, err := strconv.Atoi(args[i])
				if err != nil || n < 0 {
					return o, errors.New("--tail requires a non-negative integer or all")
				}
				o.tail = n
			}
		default:
			if strings.HasPrefix(args[i], "-") {
				return o, fmt.Errorf("unknown logs option %q", args[i])
			}
			o.nodes = append(o.nodes, args[i])
		}
	}
	return o, nil
}

func (a *App) logs(ctx context.Context, cfg *config.Config, m *state.Manifest, args []string) error {
	o, err := parseLogs(args)
	if err != nil {
		return err
	}
	names, err := selectNodes(cfg, o.nodes)
	if err != nil {
		return err
	}
	if m == nil {
		return errors.New("topology is not up")
	}
	last := map[string]int{}
	printRecords := func(initial bool) error {
		per := map[string][]observability.SerialRecord{}
		for _, name := range names {
			n := m.Nodes[name]
			if n == nil {
				continue
			}
			records, err := observability.ReadSerial(n.SerialLogPath, name, func() int {
				if initial {
					return o.tail
				}
				return -1
			}())
			if err != nil {
				return err
			}
			if !initial {
				switch {
				case last[name] < len(records):
					records = records[last[name]:]
				case last[name] == len(records):
					records = nil
				default: // the log was truncated or replaced
					last[name] = 0
				}
			}
			if initial && o.tail >= 0 {
				last[name] = countSerial(n.SerialLogPath)
			} else {
				last[name] = countSerial(n.SerialLogPath)
			}
			per[name] = records
		}
		for _, r := range observability.MergeSerial(per) {
			if o.timestamps {
				fmt.Fprintf(a.Stdout, "%s %s: %s\n", r.Timestamp.UTC().Format(time.RFC3339Nano), r.Node, r.Line)
			} else {
				fmt.Fprintf(a.Stdout, "%s: %s\n", r.Node, r.Line)
			}
		}
		return nil
	}
	if err := printRecords(true); err != nil {
		return err
	}
	if !o.follow {
		return nil
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := printRecords(false); err != nil {
				return err
			}
		}
	}
}

func countSerial(path string) int {
	records, _ := observability.ReadSerial(path, "", -1)
	return len(records)
}

func (a *App) counters(ctx context.Context, cfg *config.Config, m *state.Manifest, args []string) error {
	format, names, err := parseCounters(args)
	if err != nil {
		return err
	}
	if m == nil {
		return errors.New("topology is not up")
	}
	ns := oldNetwork(m)
	if ns == nil {
		return errors.New("managed network is absent")
	}
	if len(names) > 0 {
		selected := map[string]bool{}
		for _, n := range names {
			if _, ok := cfg.LookupLink(n); !ok {
				return fmt.Errorf("unknown link %q", n)
			}
			selected[n] = true
		}
		clone := *ns
		clone.Links = map[string]network.LinkState{}
		for n, l := range ns.Links {
			if selected[n] {
				clone.Links[n] = l
			}
		}
		ns = &clone
	}
	report, err := a.Network.Counters(ctx, ns)
	if err != nil {
		return err
	}
	if format == "json" {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Fprintln(a.Stdout, "LINK\tENDPOINT\tINGRESS_PKTS\tINGRESS_BYTES\tEGRESS_PKTS\tEGRESS_BYTES\tLOSS_DROPS\tRATE_DROPS\tGENERATION")
	for _, l := range report.Links {
		for _, e := range l.Endpoints {
			fmt.Fprintf(a.Stdout, "%s\t%s:%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.Name, e.Node, e.Interface, counter(e.Ingress.Packets), counter(e.Ingress.Bytes), counter(e.Egress.Packets), counter(e.Egress.Bytes), counter(e.Impairment.ConfiguredLossDrops), counter(e.Impairment.RateLimiterDrops), l.GenerationID)
		}
	}
	return nil
}

func parseCounters(args []string) (string, []string, error) {
	format := "table"
	var names []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--format" {
			if i+1 >= len(args) {
				return "", nil, errors.New("--format requires table or json")
			}
			i++
			format = args[i]
			if format != "table" && format != "json" {
				return "", nil, errors.New("--format requires table or json")
			}
		} else if strings.HasPrefix(args[i], "-") {
			return "", nil, fmt.Errorf("unknown counters option %q", args[i])
		} else {
			names = append(names, args[i])
		}
	}
	return format, names, nil
}
func counter(v *uint64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatUint(*v, 10)
}

type captureOptions struct {
	link, endpoint, direction, output string
	snaplen                           uint32
	duration                          time.Duration
	count                             uint64
}

func parseCapture(args []string) (captureOptions, error) {
	o := captureOptions{direction: "both", snaplen: 262144}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		var v string
		var err error
		switch arg {
		case "--endpoint":
			v, err = value()
			o.endpoint = v
		case "--direction":
			v, err = value()
			o.direction = v
		case "--output":
			v, err = value()
			o.output = v
		case "--snaplen":
			v, err = value()
			n, e := strconv.ParseUint(v, 10, 32)
			if e != nil {
				err = errors.New("invalid --snaplen")
			} else {
				o.snaplen = uint32(n)
			}
		case "--duration":
			v, err = value()
			if err == nil {
				o.duration, err = time.ParseDuration(v)
				if o.duration <= 0 {
					err = errors.New("--duration must be positive")
				}
			}
		case "--count":
			v, err = value()
			o.count, err = strconv.ParseUint(v, 10, 64)
			if err == nil && o.count == 0 {
				err = errors.New("--count must be positive")
			}
		default:
			if strings.HasPrefix(arg, "-") {
				err = fmt.Errorf("unknown capture option %q", arg)
			} else {
				positional = append(positional, arg)
			}
		}
		if err != nil {
			return o, err
		}
	}
	if len(positional) != 1 {
		return o, errors.New("capture requires exactly one LINK")
	}
	o.link = positional[0]
	if o.endpoint == "" {
		return o, errors.New("capture requires --endpoint NODE:INTERFACE")
	}
	if o.direction != "ingress" && o.direction != "egress" && o.direction != "both" {
		return o, errors.New("--direction requires ingress, egress, or both")
	}
	if o.snaplen < 64 || o.snaplen > 262144 {
		return o, errors.New("--snaplen must be from 64 through 262144")
	}
	return o, nil
}

func (a *App) capture(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, args []string) error {
	o, err := parseCapture(args)
	if err != nil {
		return err
	}
	if _, ok := cfg.LookupLink(o.link); !ok {
		return fmt.Errorf("unknown link %q", o.link)
	}
	if m == nil {
		return errors.New("topology is not up")
	}
	ns := oldNetwork(m)
	if ns == nil {
		return errors.New("managed network is absent")
	}
	if err := a.Network.Verify(ctx, ns); err != nil {
		return err
	}
	link, ok := ns.Links[o.link]
	if !ok {
		return fmt.Errorf("link %s has no running datapath", o.link)
	}
	parts := strings.SplitN(o.endpoint, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("--endpoint must be NODE:INTERFACE")
	}
	var ep *network.EndpointState
	for i := range link.Endpoints {
		if link.Endpoints[i].Node == parts[0] && link.Endpoints[i].Interface == parts[1] {
			ep = &link.Endpoints[i]
		}
	}
	if ep == nil {
		return fmt.Errorf("endpoint %s does not belong to link %s", o.endpoint, o.link)
	}
	f, path, err := observability.CreateCaptureFile(layout.WorkingDir, cfg.Name, o.link, o.output, a.Now())
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	var desc []observability.InterfaceDescription
	var sources []observability.PacketSource
	add := func(dir observability.Direction, device string) error {
		src, err := newPacketSource(device, len(desc), dir, o.snaplen)
		if err != nil {
			return err
		}
		sources = append(sources, src)
		desc = append(desc, observability.InterfaceDescription{Name: device, Direction: dir, Comment: fmt.Sprintf("topology=%s link=%s endpoint=%s direction=%s generation=%s", cfg.Name, o.link, o.endpoint, dir, link.GenerationID)})
		return nil
	}
	if err := a.Namespaces.With(ns.FabricNamespace, func() error {
		if o.direction == "ingress" || o.direction == "both" {
			if err := add(observability.Ingress, ep.IngressDevice); err != nil {
				return err
			}
		}
		if o.direction == "egress" || o.direction == "both" {
			if err := add(observability.Egress, ep.EgressDevice); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	w, err := observability.NewPcapNGWriter(f, o.snaplen, desc)
	if err != nil {
		return err
	}
	keep = true
	if err := observability.Capture(ctx, w, sources, observability.CaptureOptions{
		Duration: o.duration,
		Count:    o.count,
		OwnershipCheck: func(checkCtx context.Context) error {
			current, err := state.Load(layout.ManifestPath)
			if err != nil {
				return err
			}
			if current == nil {
				return errors.New("topology state was removed")
			}
			currentNetwork, err := manifestNetwork(current)
			if err != nil {
				return err
			}
			if currentNetwork == nil {
				return errors.New("managed network state was removed")
			}
			currentLink, ok := currentNetwork.Links[o.link]
			if !ok || currentLink.GenerationID != link.GenerationID {
				return errors.New("datapath generation changed")
			}
			return a.Network.Verify(checkCtx, currentNetwork)
		},
	}); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, path)
	return nil
}
