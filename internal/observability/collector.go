package observability

import (
	"context"
	"fmt"
	"sort"
	"time"
)

type EndpointRef struct{ Node, Interface, IngressDevice, EgressDevice, ImpairmentDevice string }
type LinkRef struct {
	Name, GenerationID  string
	GenerationStartedAt time.Time
	Endpoints           []EndpointRef
}
type CounterSource interface {
	ReadEndpoint(context.Context, EndpointRef) (EndpointCounters, error)
}
type CounterCollector struct {
	Source CounterSource
	Now    func() time.Time
}

func (c CounterCollector) Collect(ctx context.Context, topology string, links []LinkRef) (CounterReport, error) {
	if c.Source == nil {
		return CounterReport{}, fmt.Errorf("counter source is required")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	report := CounterReport{Topology: topology, CollectedAt: c.Now().UTC()}
	ordered := append([]LinkRef(nil), links...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	for _, link := range ordered {
		item := LinkCounters{Name: link.Name, GenerationID: link.GenerationID, GenerationStartedAt: link.GenerationStartedAt}
		for _, ref := range link.Endpoints {
			endpoint, err := c.Source.ReadEndpoint(ctx, ref)
			if err != nil {
				return CounterReport{}, fmt.Errorf("read %s %s:%s: %w", link.Name, ref.Node, ref.Interface, err)
			}
			endpoint.Node, endpoint.Interface = ref.Node, ref.Interface
			item.Endpoints = append(item.Endpoints, endpoint)
		}
		report.Links = append(report.Links, item)
	}
	return report, nil
}
