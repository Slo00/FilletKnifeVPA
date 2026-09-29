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

// Package victoriametrics computes resource recommendations by querying
// VictoriaMetrics directly with MetricsQL, instead of maintaining VPA's
// in-memory decaying histogram. See docs/adr/0002-percentiles-via-metricsql.md:
// the recommender is stateless, there is no checkpoint or warm-up to reproduce.
//
// CPU asymmetry (сжимаем) is expressed as a percentile over time; memory
// asymmetry (несжимаема) is expressed as a maximum over time for the target,
// with percentiles only for the softer lower/upper bounds. See
// docs/adr/0002-percentiles-via-metricsql.md.
package victoriametrics

import (
	"context"
	"fmt"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"
	"k8s.io/klog/v2"

	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input/history"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/logic"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
)

// Config describes how to reach VictoriaMetrics and which MetricsQL queries
// to run. Connection and label settings deliberately mirror
// history.PrometheusHistoryProviderConfig: one TSDB, one set of flags.
type Config struct {
	Address      string
	Insecure     bool
	QueryTimeout time.Duration

	// CadvisorJobName, if non-empty, restricts queries to job="...".
	CadvisorJobName string
	// ClusterName, if non-empty, restricts queries to cluster="...". Needed
	// when a single VictoriaMetrics instance holds metrics from multiple
	// clusters (common in a centralized-monitoring setup) - without it,
	// namespace+pod+container alone can collide across clusters.
	ClusterName    string
	NamespaceLabel string
	PodNameLabel   string
	ContainerLabel string

	CPUMetricName    string
	MemoryMetricName string
	// RequestsMetricName and LimitsMetricName are the kube-state-metrics series with
	// what containers declare; only the charts of the web page use them. Empty means
	// DefaultRequestsMetric and DefaultLimitsMetric.
	RequestsMetricName string
	LimitsMetricName   string

	// Window is how far back quantile_over_time/max_over_time look.
	// Prometheus/MetricsQL duration literal, e.g. "8d".
	Window string
	// Step is the subquery resolution used to pool multi-pod series over time.
	// e.g. "1h".
	Step string
	// RateWindow is the inner window for rate(cpu_metric[RateWindow]), e.g. "5m".
	RateWindow string

	SafetyMarginFraction float64
	PodMinCPUMillicores  float64
	PodMinMemoryMb       float64

	TargetCPUPercentile     float64
	LowerBoundCPUPercentile float64
	UpperBoundCPUPercentile float64

	TargetMemoryPercentile     float64
	LowerBoundMemoryPercentile float64
	UpperBoundMemoryPercentile float64

	Authentication history.PrometheusCredentials
}

// Recommender computes RecommendedPodResources by querying VictoriaMetrics.
type Recommender struct {
	api    prometheusv1.API
	config Config
}

// NewRecommender builds a Recommender talking to VictoriaMetrics over its
// Prometheus-compatible HTTP API. The client itself is
// history.NewPrometheusAPIClient - the same one input/history uses - so
// there's one HTTP/auth implementation for both.
func NewRecommender(config Config) (*Recommender, error) {
	api, err := history.NewPrometheusAPIClient(config.Address, config.Insecure, config.Authentication)
	if err != nil {
		return nil, fmt.Errorf("cannot create VictoriaMetrics client: %v", err)
	}
	return &Recommender{api: api, config: config}, nil
}

// GetRecommendedPodResources queries VictoriaMetrics for each of the given
// containers, scoped to pods of the given namespace whose name starts with
// podNamePrefix (the VPA's targetRef name - Deployment/StatefulSet/DaemonSet
// pods are always named "<workload>-...").
//
// Returns an error without a partial result if any query fails; the caller
// is expected to keep the previous recommendation rather than overwrite it
// with an incomplete one (see docs/adr/0001-vm-metrics-source.md and ADR-0002).
func (r *Recommender) GetRecommendedPodResources(ctx context.Context, namespace, podNamePrefix string, containerNames []string) (logic.RecommendedPodResources, error) {
	klog.V(4).InfoS("Fetching recommendation from VictoriaMetrics", "namespace", namespace, "podNamePrefix", podNamePrefix, "containers", containerNames)
	start := time.Now()

	recommendation := make(logic.RecommendedPodResources, len(containerNames))
	fraction := 1.0
	if n := len(containerNames); n > 0 {
		fraction = 1.0 / float64(n)
	}
	for _, containerName := range containerNames {
		resources, err := r.estimateContainer(ctx, namespace, podNamePrefix, containerName, fraction)
		if err != nil {
			return nil, fmt.Errorf("container %s: %v", containerName, err)
		}
		recommendation[containerName] = resources
	}

	klog.V(4).InfoS("Fetched recommendation from VictoriaMetrics", "namespace", namespace, "podNamePrefix", podNamePrefix, "duration", time.Since(start), "recommendation", recommendation)
	return recommendation, nil
}

func (r *Recommender) selector(namespace, podNamePrefix, containerName string) string {
	var job, cluster string
	if r.config.CadvisorJobName != "" {
		job = fmt.Sprintf(`job="%s", `, r.config.CadvisorJobName)
	}
	if r.config.ClusterName != "" {
		cluster = fmt.Sprintf(`cluster="%s", `, r.config.ClusterName)
	}
	return fmt.Sprintf(`%s%s%s="%s", %s=~"^%s.*", %s="%s"`,
		job, cluster,
		r.config.NamespaceLabel, namespace,
		r.config.PodNameLabel, podNamePrefix,
		r.config.ContainerLabel, containerName,
	)
}

// cpuQuantileQuery pools all matching pods at every point in time
// (quantile(...)), then takes the percentile of that pooled series over the
// window (quantile_over_time(...)). This is an approximation of a true
// pooled-samples percentile (see ADR-0002, "Последствия" - затухание/окно
// остаются открытым вопросом), but it's exact when a single pod matches.
func (r *Recommender) cpuQuantileQuery(selector string, percentile float64) string {
	return fmt.Sprintf(
		`quantile_over_time(%g, quantile(%g, rate(%s{%s}[%s]))[%s:%s])`,
		percentile, percentile, r.config.CPUMetricName, selector,
		r.config.RateWindow, r.config.Window, r.config.Step,
	)
}

// memoryMaxQuery takes the true maximum over all pods and all time: max of
// per-pod maxes is exactly the global max, no approximation involved.
func (r *Recommender) memoryMaxQuery(selector string) string {
	return fmt.Sprintf(`max(max_over_time(%s{%s}[%s]))`, r.config.MemoryMetricName, selector, r.config.Window)
}

func (r *Recommender) memoryQuantileQuery(selector string, percentile float64) string {
	return fmt.Sprintf(
		`quantile_over_time(%g, quantile(%g, %s{%s})[%s:%s])`,
		percentile, percentile, r.config.MemoryMetricName, selector,
		r.config.Window, r.config.Step,
	)
}

func (r *Recommender) queryScalar(ctx context.Context, expr string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.config.QueryTimeout)
	defer cancel()

	klog.V(4).InfoS("Querying VictoriaMetrics", "query", expr)
	start := time.Now()

	result, warnings, err := r.api.Query(ctx, expr, time.Now())
	duration := time.Since(start)
	if err != nil {
		klog.V(4).ErrorS(err, "VictoriaMetrics query failed", "query", expr, "duration", duration)
		return 0, fmt.Errorf("query %q: %v", expr, err)
	}
	if len(warnings) > 0 {
		klog.V(4).InfoS("VictoriaMetrics query returned warnings", "query", expr, "warnings", warnings)
	}

	vector, ok := result.(prommodel.Vector)
	if !ok {
		return 0, fmt.Errorf("query %q: expected an instant vector, got %T", expr, result)
	}
	if len(vector) == 0 {
		// No data yet (cold start, no traffic so far). 0 is a safe base: the
		// PodMinCPUMillicores/PodMinMemoryMb floor below still applies.
		klog.V(4).InfoS("VictoriaMetrics query returned no data", "query", expr, "duration", duration)
		return 0, nil
	}
	klog.V(4).InfoS("VictoriaMetrics query result", "query", expr, "value", float64(vector[0].Value), "duration", duration)
	return float64(vector[0].Value), nil
}

func withMargin(amount model.ResourceAmount, marginFraction float64) model.ResourceAmount {
	return amount + model.ScaleResource(amount, marginFraction)
}

func (r *Recommender) estimateContainer(ctx context.Context, namespace, podNamePrefix, containerName string, containerFraction float64) (logic.RecommendedContainerResources, error) {
	selector := r.selector(namespace, podNamePrefix, containerName)

	targetCPU, err := r.queryScalar(ctx, r.cpuQuantileQuery(selector, r.config.TargetCPUPercentile))
	if err != nil {
		return logic.RecommendedContainerResources{}, fmt.Errorf("target CPU: %v", err)
	}
	lowerCPU, err := r.queryScalar(ctx, r.cpuQuantileQuery(selector, r.config.LowerBoundCPUPercentile))
	if err != nil {
		return logic.RecommendedContainerResources{}, fmt.Errorf("lower bound CPU: %v", err)
	}
	upperCPU, err := r.queryScalar(ctx, r.cpuQuantileQuery(selector, r.config.UpperBoundCPUPercentile))
	if err != nil {
		return logic.RecommendedContainerResources{}, fmt.Errorf("upper bound CPU: %v", err)
	}

	targetMemory, err := r.queryScalar(ctx, r.memoryMaxQuery(selector))
	if err != nil {
		return logic.RecommendedContainerResources{}, fmt.Errorf("target memory: %v", err)
	}
	lowerMemory, err := r.queryScalar(ctx, r.memoryQuantileQuery(selector, r.config.LowerBoundMemoryPercentile))
	if err != nil {
		return logic.RecommendedContainerResources{}, fmt.Errorf("lower bound memory: %v", err)
	}
	upperMemory, err := r.queryScalar(ctx, r.memoryQuantileQuery(selector, r.config.UpperBoundMemoryPercentile))
	if err != nil {
		return logic.RecommendedContainerResources{}, fmt.Errorf("upper bound memory: %v", err)
	}

	minCPU := model.CPUAmountFromCores(r.config.PodMinCPUMillicores * 0.001 * containerFraction)
	minMemory := model.MemoryAmountFromBytes(r.config.PodMinMemoryMb * 1024 * 1024 * containerFraction)

	cpu := func(cores float64) model.ResourceAmount {
		return model.ResourceAmountMax(withMargin(model.CPUAmountFromCores(cores), r.config.SafetyMarginFraction), minCPU)
	}
	memory := func(bytes float64) model.ResourceAmount {
		return model.ResourceAmountMax(withMargin(model.MemoryAmountFromBytes(bytes), r.config.SafetyMarginFraction), minMemory)
	}

	return logic.RecommendedContainerResources{
		Target: model.Resources{
			model.ResourceCPU:    cpu(targetCPU),
			model.ResourceMemory: memory(targetMemory),
		},
		LowerBound: model.Resources{
			model.ResourceCPU:    cpu(lowerCPU),
			model.ResourceMemory: memory(lowerMemory),
		},
		UpperBound: model.Resources{
			model.ResourceCPU:    cpu(upperCPU),
			model.ResourceMemory: memory(upperMemory),
		},
	}, nil
}
