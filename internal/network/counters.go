package network

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type linkStats struct {
	Stats64 struct {
		RX struct{ Bytes, Packets, Errors, Dropped uint64 }
		TX struct{ Bytes, Packets, Errors, Dropped uint64 }
	} `json:"stats64"`
}

type tcStats struct {
	Kind       string         `json:"kind"`
	Handle     string         `json:"handle"`
	Parent     string         `json:"parent"`
	Drops      *uint64        `json:"drops"`
	Dropped    *uint64        `json:"dropped"` // netem loss-model xstat
	Overlimits *uint64        `json:"overlimits"`
	Requeues   *uint64        `json:"requeues"`
	Backlog    *uint64        `json:"backlog"`
	QLen       *uint64        `json:"qlen"`
	Bytes      *uint64        `json:"bytes"`
	Packets    *uint64        `json:"packets"`
	Options    map[string]any `json:"options"`
}

type tcFilter struct {
	Options struct {
		Actions []struct {
			Stats struct {
				Bytes   *uint64 `json:"bytes"`
				Packets *uint64 `json:"packets"`
			} `json:"stats"`
		} `json:"actions"`
	} `json:"options"`
}

func (m *Manager) Counters(ctx context.Context, state *State) (*CounterSnapshot, error) {
	if state == nil {
		return nil, fmt.Errorf("network: nil state")
	}
	if err := m.Verify(ctx, state); err != nil {
		return nil, err
	}
	result := &CounterSnapshot{Topology: state.Topology, Collected: m.now().UTC()}
	names := make([]string, 0, len(state.Links))
	for name := range state.Links {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		link := state.Links[name]
		ns := state.FabricNamespace
		lc := LinkCounter{Name: name, GenerationID: link.GenerationID, GenerationStartedAt: link.GenerationStartedAt}
		for _, ep := range link.Endpoints {
			ingress, err := m.readLinkStats(ctx, ns, ep.IngressDevice)
			if err != nil {
				return nil, err
			}
			egress, err := m.readLinkStats(ctx, ns, ep.EgressDevice)
			if err != nil {
				return nil, err
			}
			impairment, err := m.readLinkStats(ctx, ns, ep.ImpairmentDevice)
			if err != nil {
				return nil, err
			}
			qdiscs, err := m.readTC(ctx, ns, "qdisc", ep.ImpairmentDevice)
			if err != nil {
				return nil, err
			}
			classes, err := m.readTC(ctx, ns, "class", ep.ImpairmentDevice)
			if err != nil {
				return nil, err
			}
			offeredPackets, offeredBytes, err := m.readOffered(ctx, ns, ep.ImpairmentDevice)
			if err != nil {
				return nil, err
			}
			ec := EndpointCounter{Node: ep.Node, Interface: ep.Interface}
			ec.Ingress = TrafficCounters{Packets: ptr(ingress.Stats64.RX.Packets), Bytes: ptr(ingress.Stats64.RX.Bytes)}
			ec.Egress = TrafficCounters{Packets: ptr(egress.Stats64.RX.Packets), Bytes: ptr(egress.Stats64.RX.Bytes)}
			ec.Impairment.OfferedPackets = offeredPackets
			ec.Impairment.OfferedBytes = offeredBytes
			ec.Impairment.DeliveredPackets = ptr(egress.Stats64.RX.Packets)
			ec.Impairment.DeliveredBytes = ptr(egress.Stats64.RX.Bytes)
			ec.Device = DeviceCounters{RXErrors: ptr(impairment.Stats64.RX.Errors), RXDropped: ptr(impairment.Stats64.RX.Dropped), TXErrors: ptr(impairment.Stats64.TX.Errors), TXDropped: ptr(impairment.Stats64.TX.Dropped)}
			fillTC(&ec.Impairment, qdiscs, classes)
			lc.Endpoints = append(lc.Endpoints, ec)
		}
		result.Links = append(result.Links, lc)
	}
	return result, nil
}

func (m *Manager) readOffered(ctx context.Context, ns, device string) (*uint64, *uint64, error) {
	out, err := m.runIn(ctx, ns, "tc", "-j", "-s", "filter", "show", "dev", device, "egress")
	if err != nil {
		return nil, nil, err
	}
	var rows []tcFilter
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, nil, fmt.Errorf("network: decode offered counter for %s: %w", device, err)
	}
	var packets, bytes uint64
	foundPackets, foundBytes := false, false
	for _, row := range rows {
		for _, action := range row.Options.Actions {
			if action.Stats.Packets != nil {
				packets += *action.Stats.Packets
				foundPackets = true
			}
			if action.Stats.Bytes != nil {
				bytes += *action.Stats.Bytes
				foundBytes = true
			}
		}
	}
	var packetPtr, bytePtr *uint64
	if foundPackets {
		packetPtr = &packets
	}
	if foundBytes {
		bytePtr = &bytes
	}
	return packetPtr, bytePtr, nil
}

func (m *Manager) readLinkStats(ctx context.Context, ns, device string) (linkStats, error) {
	out, err := m.runIn(ctx, ns, "ip", "-j", "-s", "link", "show", "dev", device)
	if err != nil {
		return linkStats{}, err
	}
	var rows []linkStats
	if err := json.Unmarshal(out, &rows); err != nil {
		return linkStats{}, fmt.Errorf("network: decode counters for %s: %w", device, err)
	}
	if len(rows) != 1 {
		return linkStats{}, fmt.Errorf("network: counters for %s returned %d devices", device, len(rows))
	}
	return rows[0], nil
}

func (m *Manager) readTC(ctx context.Context, ns, object, device string) ([]tcStats, error) {
	out, err := m.runIn(ctx, ns, "tc", "-j", "-s", object, "show", "dev", device)
	if err != nil {
		return nil, err
	}
	var rows []tcStats
	if len(out) == 0 {
		return rows, nil
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("network: decode tc %s for %s: %w", object, device, err)
	}
	return rows, nil
}

func fillTC(out *ImpairmentCounters, qdiscs, classes []tcStats) {
	var lossDrops, queueDrops *uint64
	for _, q := range qdiscs {
		switch q.Kind {
		case "netem":
			// TopoLab owns handle 10: exclusively for the non-queueing loss
			// stage and 20: exclusively for delay/jitter queueing.
			switch q.Handle {
			case "10:":
				// Prefer the dedicated loss-model xstat when iproute exposes
				// it. Some kernels expose only the generic drop count; retain
				// that as a fallback and remove propagated child queue drops
				// after all qdiscs have been visited.
				if q.Dropped != nil {
					out.ConfiguredLossDrops = q.Dropped
				} else {
					lossDrops = q.Drops
				}
			case "20:":
				out.NetemQueueDrops = q.Drops
				queueDrops = q.Drops
			}
			if q.Handle == "20:" && q.Backlog != nil {
				out.BacklogBytes = q.Backlog
			}
			if q.Handle == "20:" && q.QLen != nil {
				out.BacklogPackets = q.QLen
			}
		case "htb":
			// Generic HTB drops include enqueue failures propagated by child
			// qdiscs, so they cannot be attributed to rate limiting.
			out.RateLimiterOverlimits = q.Overlimits
			out.RateLimiterRequeues = q.Requeues
		}
	}
	if out.ConfiguredLossDrops == nil && lossDrops != nil {
		configured := *lossDrops
		if queueDrops != nil && *queueDrops <= configured {
			configured -= *queueDrops
		}
		out.ConfiguredLossDrops = &configured
	}
	for _, c := range classes {
		if c.Kind != "htb" {
			continue
		}
		out.RateLimiterOverlimits = c.Overlimits
		out.RateLimiterRequeues = c.Requeues
		if c.Backlog != nil {
			out.BacklogBytes = c.Backlog
		}
		if c.QLen != nil {
			out.BacklogPackets = c.QLen
		}
	}
}

func ptr(v uint64) *uint64 { return &v }

var _ = time.Time{} // preserve explicit time-unit documentation in this file
