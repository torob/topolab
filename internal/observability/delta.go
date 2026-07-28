package observability

import (
	"errors"
	"fmt"
)

func deltaReports(before, after CounterReport) (CounterReport, error) {
	if before.Topology != after.Topology {
		return CounterReport{}, errors.New("counter topology changed")
	}
	out := after
	bm := map[string]LinkCounters{}
	for _, l := range before.Links {
		bm[l.Name] = l
	}
	for li := range out.Links {
		a := &out.Links[li]
		b, ok := bm[a.Name]
		if !ok {
			return CounterReport{}, fmt.Errorf("link %s missing from before snapshot", a.Name)
		}
		if b.GenerationID != a.GenerationID {
			return CounterReport{}, fmt.Errorf("link %s generation changed", a.Name)
		}
		if len(b.Endpoints) != len(a.Endpoints) {
			return CounterReport{}, fmt.Errorf("link %s endpoints changed", a.Name)
		}
		for ei := range a.Endpoints {
			if err := deltaEndpoint(&a.Endpoints[ei], b.Endpoints[ei]); err != nil {
				return CounterReport{}, fmt.Errorf("link %s: %w", a.Name, err)
			}
		}
	}
	return out, nil
}
func deltaEndpoint(a *EndpointCounters, b EndpointCounters) error {
	if a.Node != b.Node || a.Interface != b.Interface {
		return errors.New("endpoint order changed")
	}
	pairs := [][2]*CounterValue{{&a.Ingress.Packets, &b.Ingress.Packets}, {&a.Ingress.Bytes, &b.Ingress.Bytes}, {&a.Egress.Packets, &b.Egress.Packets}, {&a.Egress.Bytes, &b.Egress.Bytes}, {&a.Impairment.OfferedPackets, &b.Impairment.OfferedPackets}, {&a.Impairment.OfferedBytes, &b.Impairment.OfferedBytes}, {&a.Impairment.DeliveredPackets, &b.Impairment.DeliveredPackets}, {&a.Impairment.DeliveredBytes, &b.Impairment.DeliveredBytes}, {&a.Impairment.ConfiguredLossDrops, &b.Impairment.ConfiguredLossDrops}, {&a.Impairment.NetemQueueDrops, &b.Impairment.NetemQueueDrops}, {&a.Impairment.RateLimiterDrops, &b.Impairment.RateLimiterDrops}, {&a.Impairment.RateLimiterOverlimits, &b.Impairment.RateLimiterOverlimits}, {&a.Impairment.RateLimiterRequeues, &b.Impairment.RateLimiterRequeues}, {&a.Impairment.RateLimiterBacklogPackets, &b.Impairment.RateLimiterBacklogPackets}, {&a.Impairment.RateLimiterBacklogBytes, &b.Impairment.RateLimiterBacklogBytes}, {&a.Device.RXErrors, &b.Device.RXErrors}, {&a.Device.RXDrops, &b.Device.RXDrops}, {&a.Device.TXErrors, &b.Device.TXErrors}, {&a.Device.TXDrops, &b.Device.TXDrops}, {&a.Device.MTUDrops, &b.Device.MTUDrops}}
	for _, p := range pairs {
		if *p[0] == nil || *p[1] == nil {
			*p[0] = nil
			continue
		}
		if **p[0] < **p[1] {
			return errors.New("counter decreased")
		}
		v := **p[0] - **p[1]
		*p[0] = &v
	}
	return nil
}
