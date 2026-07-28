package observability

import "time"

type CounterValue = *uint64
type TrafficCounters struct {
	Packets CounterValue `json:"packets"`
	Bytes   CounterValue `json:"bytes"`
}
type ImpairmentCounters struct {
	OfferedPackets            CounterValue `json:"offered_packets"`
	OfferedBytes              CounterValue `json:"offered_bytes"`
	DeliveredPackets          CounterValue `json:"delivered_packets"`
	DeliveredBytes            CounterValue `json:"delivered_bytes"`
	ConfiguredLossDrops       CounterValue `json:"configured_loss_drops"`
	NetemQueueDrops           CounterValue `json:"netem_queue_drops"`
	RateLimiterDrops          CounterValue `json:"rate_limiter_drops"`
	RateLimiterOverlimits     CounterValue `json:"rate_limiter_overlimits"`
	RateLimiterRequeues       CounterValue `json:"rate_limiter_requeues"`
	RateLimiterBacklogPackets CounterValue `json:"rate_limiter_backlog_packets"`
	RateLimiterBacklogBytes   CounterValue `json:"rate_limiter_backlog_bytes"`
}
type DeviceCounters struct {
	RXErrors CounterValue `json:"rx_errors"`
	RXDrops  CounterValue `json:"rx_drops"`
	TXErrors CounterValue `json:"tx_errors"`
	TXDrops  CounterValue `json:"tx_drops"`
	MTUDrops CounterValue `json:"mtu_drops"`
}
type EndpointCounters struct {
	Node       string             `json:"node"`
	Interface  string             `json:"interface"`
	Ingress    TrafficCounters    `json:"ingress"`
	Egress     TrafficCounters    `json:"egress"`
	Impairment ImpairmentCounters `json:"impairment"`
	Device     DeviceCounters     `json:"device"`
}
type LinkCounters struct {
	Name                string             `json:"name"`
	GenerationID        string             `json:"generation_id"`
	GenerationStartedAt time.Time          `json:"generation_started_at"`
	Endpoints           []EndpointCounters `json:"endpoints"`
}
type CounterReport struct {
	Topology    string         `json:"topology"`
	CollectedAt time.Time      `json:"collected_at"`
	Links       []LinkCounters `json:"links"`
}

func U64(v uint64) *uint64 { return &v }

func Delta(before, after CounterReport) (CounterReport, error) { return deltaReports(before, after) }
