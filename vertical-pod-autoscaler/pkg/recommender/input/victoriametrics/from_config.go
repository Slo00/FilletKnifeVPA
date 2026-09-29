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
	"fmt"
	"time"

	recommender_config "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/config"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input/history"
)

// DefaultRateWindow is the inner rate() window used for CPU usage. Not exposed as
// a flag yet - revisit together with the ADR-0002 open question on windowing.
const DefaultRateWindow = "5m"

// Option adjusts the Config that NewRecommenderFromConfig derives from the flags.
type Option func(*Config)

// WithResourceMetrics names the series with the requests and limits containers
// declare, for the charts. Empty keeps the default.
func WithResourceMetrics(requests, limits string) Option {
	return func(c *Config) {
		c.RequestsMetricName = requests
		c.LimitsMetricName = limits
	}
}

// NewRecommenderFromConfig builds a Recommender from the recommender flags. The
// recommender and the web UI both use it, so the UI shows exactly the numbers the
// recommender would calculate when started with the same flags.
func NewRecommenderFromConfig(config *recommender_config.RecommenderConfig, opts ...Option) (*Recommender, error) {
	queryTimeout, err := time.ParseDuration(config.QueryTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid --prometheus-query-timeout: %v", err)
	}
	cfg := Config{
		Address:          config.PrometheusAddress,
		Insecure:         config.PrometheusInsecure,
		QueryTimeout:     queryTimeout,
		CadvisorJobName:  config.PrometheusJobName,
		ClusterName:      config.ClusterName,
		NamespaceLabel:   config.CtrNamespaceLabel,
		PodNameLabel:     config.CtrPodNameLabel,
		ContainerLabel:   config.CtrNameLabel,
		CPUMetricName:    config.HistoryCPUMetric,
		MemoryMetricName: config.HistoryMemoryMetric,
		Window:           config.HistoryLength,
		Step:             config.HistoryResolution,
		RateWindow:       DefaultRateWindow,

		SafetyMarginFraction:       config.SafetyMarginFraction,
		PodMinCPUMillicores:        config.PodMinCPUMillicores,
		PodMinMemoryMb:             config.PodMinMemoryMb,
		TargetCPUPercentile:        config.TargetCPUPercentile,
		LowerBoundCPUPercentile:    config.LowerBoundCPUPercentile,
		UpperBoundCPUPercentile:    config.UpperBoundCPUPercentile,
		TargetMemoryPercentile:     config.TargetMemoryPercentile,
		LowerBoundMemoryPercentile: config.LowerBoundMemoryPercentile,
		UpperBoundMemoryPercentile: config.UpperBoundMemoryPercentile,

		Authentication: history.PrometheusCredentials{
			BearerToken: config.PrometheusBearerToken,
			Username:    config.Username,
			Password:    config.Password,
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return NewRecommender(cfg)
}
