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
)

// Usage is the current consumption of one container, aggregated over its pods.
// Pods == 0 means VictoriaMetrics has no series for it (the other fields are 0 then).
type Usage struct {
	Pods           int     `json:"pods"`
	CPUCoresAvg    float64 `json:"cpuCoresAvg"`
	CPUCoresMax    float64 `json:"cpuCoresMax"`
	MemoryBytesAvg float64 `json:"memoryBytesAvg"`
	MemoryBytesMax float64 `json:"memoryBytesMax"`
}

// GetCurrentUsage reports what the container consumes right now: CPU as a rate over
// RateWindow, memory as the working set, both averaged and maxed over its pods.
func (r *Recommender) GetCurrentUsage(ctx context.Context, namespace, podNamePrefix, containerName string) (Usage, error) {
	selector := r.selector(namespace, podNamePrefix, containerName)
	cpuRate := fmt.Sprintf(`rate(%s{%s}[%s])`, r.config.CPUMetricName, selector, r.config.RateWindow)
	memory := fmt.Sprintf(`%s{%s}`, r.config.MemoryMetricName, selector)

	var usage Usage
	pods, err := r.queryScalar(ctx, fmt.Sprintf(`count(%s)`, memory))
	if err != nil {
		return Usage{}, fmt.Errorf("pods: %v", err)
	}
	usage.Pods = int(pods)
	if usage.Pods == 0 {
		return usage, nil
	}

	for _, q := range []struct {
		into *float64
		expr string
		what string
	}{
		{&usage.CPUCoresAvg, fmt.Sprintf(`avg(%s)`, cpuRate), "average CPU"},
		{&usage.CPUCoresMax, fmt.Sprintf(`max(%s)`, cpuRate), "max CPU"},
		{&usage.MemoryBytesAvg, fmt.Sprintf(`avg(%s)`, memory), "average memory"},
		{&usage.MemoryBytesMax, fmt.Sprintf(`max(%s)`, memory), "max memory"},
	} {
		v, err := r.queryScalar(ctx, q.expr)
		if err != nil {
			return Usage{}, fmt.Errorf("%s: %v", q.what, err)
		}
		*q.into = v
	}
	return usage, nil
}
