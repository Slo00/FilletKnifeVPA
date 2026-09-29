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
	"testing"

	"github.com/stretchr/testify/assert"
)

func testRecommender(edit func(*Config)) *Recommender {
	config := Config{
		CadvisorJobName:  "kubernetes-nodes-cadvisor",
		NamespaceLabel:   "namespace",
		PodNameLabel:     "pod",
		ContainerLabel:   "container",
		CPUMetricName:    "container_cpu_usage_seconds_total",
		MemoryMetricName: "container_memory_working_set_bytes",
		Window:           "8d",
		Step:             "1h",
		RateWindow:       "5m",
	}
	if edit != nil {
		edit(&config)
	}
	return &Recommender{config: config}
}

func TestSelector(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"job only": {
			want: `job="kubernetes-nodes-cadvisor", namespace="default", pod=~"^hamster.*", container="hamster"`,
		},
		"with cluster": {
			edit: func(c *Config) { c.ClusterName = "prod-1" },
			want: `job="kubernetes-nodes-cadvisor", cluster="prod-1", namespace="default", pod=~"^hamster.*", container="hamster"`,
		},
		"no job": {
			edit: func(c *Config) { c.CadvisorJobName = "" },
			want: `namespace="default", pod=~"^hamster.*", container="hamster"`,
		},
		"no job, with cluster": {
			edit: func(c *Config) { c.CadvisorJobName = ""; c.ClusterName = "prod-1" },
			want: `cluster="prod-1", namespace="default", pod=~"^hamster.*", container="hamster"`,
		},
		"custom labels": {
			edit: func(c *Config) { c.NamespaceLabel = "ns"; c.PodNameLabel = "pod_name"; c.ContainerLabel = "name" },
			want: `job="kubernetes-nodes-cadvisor", ns="default", pod_name=~"^hamster.*", name="hamster"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, testRecommender(tc.edit).selector("default", "hamster", "hamster"))
		})
	}
}

func TestQueries(t *testing.T) {
	r := testRecommender(nil)
	sel := `job="j"`

	assert.Equal(t,
		`quantile_over_time(0.9, quantile(0.9, rate(container_cpu_usage_seconds_total{job="j"}[5m]))[8d:1h])`,
		r.cpuQuantileQuery(sel, 0.9))
	assert.Equal(t,
		`max(max_over_time(container_memory_working_set_bytes{job="j"}[8d]))`,
		r.memoryMaxQuery(sel))
	assert.Equal(t,
		`quantile_over_time(0.95, quantile(0.95, container_memory_working_set_bytes{job="j"})[8d:1h])`,
		r.memoryQuantileQuery(sel, 0.95))
}

func TestWithMargin(t *testing.T) {
	assert.Equal(t, int64(1150), int64(withMargin(1000, 0.15)))
	assert.Equal(t, int64(1300), int64(withMargin(1000, 0.30)))
	assert.Equal(t, int64(0), int64(withMargin(0, 0.30)))
}

func TestHistoryQueries(t *testing.T) {
	r := testRecommender(nil)
	q := r.historyQueries("default", "hamster", "hamster")

	// Usage comes from cAdvisor, so it carries the job; requests and limits come from
	// kube-state-metrics, whose job is another one, so they must not.
	assert.Equal(t,
		`max(rate(container_cpu_usage_seconds_total{job="kubernetes-nodes-cadvisor", namespace="default", pod=~"^hamster.*", container="hamster"}[5m]))`,
		q["cpu"]["usage"])
	assert.Equal(t,
		`max(container_memory_working_set_bytes{job="kubernetes-nodes-cadvisor", namespace="default", pod=~"^hamster.*", container="hamster"})`,
		q["memory"]["usage"])
	assert.Equal(t,
		`max(kube_pod_container_resource_requests{namespace="default", pod=~"^hamster.*", container="hamster", resource="cpu"})`,
		q["cpu"]["requests"])
	assert.Equal(t,
		`max(kube_pod_container_resource_limits{namespace="default", pod=~"^hamster.*", container="hamster", resource="memory"})`,
		q["memory"]["limits"])
}

func TestHistoryQueriesCustomMetricsAndCluster(t *testing.T) {
	r := testRecommender(func(c *Config) {
		c.ClusterName = "prod-1"
		c.RequestsMetricName = "my_requests"
		c.LimitsMetricName = "my_limits"
	})
	q := r.historyQueries("default", "hamster", "hamster")
	assert.Equal(t,
		`max(my_requests{cluster="prod-1", namespace="default", pod=~"^hamster.*", container="hamster", resource="cpu"})`,
		q["cpu"]["requests"])
	assert.Equal(t,
		`max(my_limits{cluster="prod-1", namespace="default", pod=~"^hamster.*", container="hamster", resource="memory"})`,
		q["memory"]["limits"])
	assert.Contains(t, q["cpu"]["usage"], `cluster="prod-1"`, "usage is scoped to the cluster too")
}
