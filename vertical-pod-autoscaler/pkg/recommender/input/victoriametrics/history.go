/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package victoriametrics

import (
	"context"
	"fmt"
	"math"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"
	"k8s.io/klog/v2"
)

const (
	// DefaultRequestsMetric and DefaultLimitsMetric are the kube-state-metrics series
	// with what a container declares. cAdvisor has no memory request and no exact CPU
	// request, so kube-state-metrics has to be scraped into the same VictoriaMetrics.
	DefaultRequestsMetric = "kube_pod_container_resource_requests"
	DefaultLimitsMetric   = "kube_pod_container_resource_limits"
)

// Point is one sample: unix seconds and the value. It marshals to [t, v].
type Point [2]float64

// Series is samples in time order. Empty means VictoriaMetrics has none.
type Series []Point

// ResourceHistory is what one resource of a container did over a period, aggregated
// as the maximum over its pods: the largest consumer is the one a limit has to fit,
// and during a rollout the largest request is the one that has been applied.
// CPU is in cores, memory in bytes.
type ResourceHistory struct {
	Usage    Series `json:"usage"`
	Requests Series `json:"requests"`
	Limits   Series `json:"limits"`
}

// History is the usage, requests and limits of a container over a period.
type History struct {
	CPU    ResourceHistory `json:"cpu"`
	Memory ResourceHistory `json:"memory"`
}

// declared is the selector of a kube-state-metrics series. It has no job (that one
// belongs to the cAdvisor scrape) and picks the resource by its label.
func (r *Recommender) declared(namespace, podNamePrefix, containerName, resource string) string {
	var cluster string
	if r.config.ClusterName != "" {
		cluster = fmt.Sprintf(`cluster="%s", `, r.config.ClusterName)
	}
	return fmt.Sprintf(`%s%s="%s", %s=~"^%s.*", %s="%s", resource="%s"`,
		cluster,
		r.config.NamespaceLabel, namespace,
		r.config.PodNameLabel, podNamePrefix,
		r.config.ContainerLabel, containerName,
		resource,
	)
}

func (r *Recommender) requestsMetric() string {
	if r.config.RequestsMetricName != "" {
		return r.config.RequestsMetricName
	}
	return DefaultRequestsMetric
}

func (r *Recommender) limitsMetric() string {
	if r.config.LimitsMetricName != "" {
		return r.config.LimitsMetricName
	}
	return DefaultLimitsMetric
}

// historyQueries are the six range queries behind a chart, by resource and series.
func (r *Recommender) historyQueries(namespace, podNamePrefix, containerName string) map[string]map[string]string {
	usage := r.selector(namespace, podNamePrefix, containerName)
	return map[string]map[string]string{
		"cpu": {
			"usage":    fmt.Sprintf(`max(rate(%s{%s}[%s]))`, r.config.CPUMetricName, usage, r.config.RateWindow),
			"requests": fmt.Sprintf(`max(%s{%s})`, r.requestsMetric(), r.declared(namespace, podNamePrefix, containerName, "cpu")),
			"limits":   fmt.Sprintf(`max(%s{%s})`, r.limitsMetric(), r.declared(namespace, podNamePrefix, containerName, "cpu")),
		},
		"memory": {
			"usage":    fmt.Sprintf(`max(%s{%s})`, r.config.MemoryMetricName, usage),
			"requests": fmt.Sprintf(`max(%s{%s})`, r.requestsMetric(), r.declared(namespace, podNamePrefix, containerName, "memory")),
			"limits":   fmt.Sprintf(`max(%s{%s})`, r.limitsMetric(), r.declared(namespace, podNamePrefix, containerName, "memory")),
		},
	}
}

// GetHistory returns usage, requests and limits of the container between start and end,
// one sample per step.
func (r *Recommender) GetHistory(ctx context.Context, namespace, podNamePrefix, containerName string, start, end time.Time, step time.Duration) (History, error) {
	queries := r.historyQueries(namespace, podNamePrefix, containerName)
	rng := prometheusv1.Range{Start: start, End: end, Step: step}

	var h History
	for _, target := range []struct {
		into     *ResourceHistory
		resource string
	}{{&h.CPU, "cpu"}, {&h.Memory, "memory"}} {
		for _, s := range []struct {
			into *Series
			name string
		}{{&target.into.Usage, "usage"}, {&target.into.Requests, "requests"}, {&target.into.Limits, "limits"}} {
			series, err := r.queryRange(ctx, queries[target.resource][s.name], rng)
			if err != nil {
				return History{}, fmt.Errorf("%s %s: %v", target.resource, s.name, err)
			}
			*s.into = series
		}
	}
	return h, nil
}

func (r *Recommender) queryRange(ctx context.Context, expr string, rng prometheusv1.Range) (Series, error) {
	ctx, cancel := context.WithTimeout(ctx, r.config.QueryTimeout)
	defer cancel()

	klog.V(4).InfoS("Querying VictoriaMetrics range", "query", expr, "start", rng.Start, "end", rng.End, "step", rng.Step)
	began := time.Now()
	result, warnings, err := r.api.QueryRange(ctx, expr, rng)
	if err != nil {
		return nil, fmt.Errorf("query %q: %v", expr, err)
	}
	if len(warnings) > 0 {
		klog.V(4).InfoS("VictoriaMetrics range query returned warnings", "query", expr, "warnings", warnings)
	}
	matrix, ok := result.(prommodel.Matrix)
	if !ok {
		return nil, fmt.Errorf("query %q: expected a matrix, got %T", expr, result)
	}
	klog.V(4).InfoS("VictoriaMetrics range query result", "query", expr, "series", len(matrix), "duration", time.Since(began))
	if len(matrix) == 0 {
		return nil, nil
	}
	return toSeries(matrix[0].Values), nil
}

// toSeries converts samples, dropping NaN and infinities: they cannot be drawn.
func toSeries(samples []prommodel.SamplePair) Series {
	series := make(Series, 0, len(samples))
	for _, s := range samples {
		v := float64(s.Value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		series = append(series, Point{float64(s.Timestamp.Unix()), v})
	}
	return series
}
