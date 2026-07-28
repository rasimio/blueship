package core

import "context"

// MetricType is one Prometheus text-exposition metric family type supported
// by BlueShip's host metrics seam.
type MetricType string

const (
	MetricGauge   MetricType = "gauge"
	MetricCounter MetricType = "counter"
)

// MetricSample is one current value supplied by the embedding host. Samples
// with the same Name must also have the same Type and Help text; Labels
// distinguish individual series within that family.
type MetricSample struct {
	Name   string
	Help   string
	Type   MetricType
	Labels map[string]string
	Value  float64
}

// MetricSampleCollector returns the host-owned samples to append to the
// framework's Prometheus scrape. It is called once per /metrics request.
type MetricSampleCollector func(context.Context) ([]MetricSample, error)
