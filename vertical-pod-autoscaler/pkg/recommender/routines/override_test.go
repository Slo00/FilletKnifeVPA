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

package routines

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"

	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/logic"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/override"
)

var (
	now     = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	expires = now.Add(48 * time.Hour)
)

// calculatedRecommender stands in for the calculation and counts how often it ran.
type calculatedRecommender struct{ calls int }

func (c *calculatedRecommender) GetRecommendedPodResources(containers model.ContainerNameToAggregateStateMap) logic.RecommendedPodResources {
	c.calls++
	out := make(logic.RecommendedPodResources)
	for name := range containers {
		r := model.Resources{model.ResourceCPU: 100, model.ResourceMemory: 100 << 20}
		out[name] = logic.RecommendedContainerResources{Target: r, LowerBound: r, UpperBound: r}
	}
	return out
}

func pin(t *testing.T, resources map[string]corev1.ResourceList) *override.Override {
	t.Helper()
	ov := &override.Override{Mode: override.ModePinned, Resources: resources, SetBy: "alice", SetAt: now, ExpiresAt: expires}
	require.NoError(t, ov.Validate())
	return ov
}

func quantities(cpu, memory string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
}

func containers(names ...string) model.ContainerNameToAggregateStateMap {
	m := make(model.ContainerNameToAggregateStateMap)
	for _, n := range names {
		m[n] = nil
	}
	return m
}

func newVpa(annotations map[string]string) *model.Vpa {
	vpa := model.NewVpa(model.VpaID{Namespace: "default", VpaName: "hamster-vpa"}, labels.Nothing(), now)
	vpa.Annotations = annotations
	return vpa
}

func TestActiveOverride(t *testing.T) {
	valid, err := pin(t, map[string]corev1.ResourceList{"app": quantities("700m", "300Mi")}).ToAnnotations()
	require.NoError(t, err)

	t.Run("none", func(t *testing.T) {
		assert.Nil(t, activeOverride(newVpa(nil), now))
	})
	t.Run("active", func(t *testing.T) {
		ov := activeOverride(newVpa(valid), now.Add(time.Hour))
		require.NotNil(t, ov)
		assert.Equal(t, "alice", ov.SetBy)
	})
	t.Run("expired goes back to auto", func(t *testing.T) {
		assert.Nil(t, activeOverride(newVpa(valid), expires))
	})
	t.Run("malformed goes back to auto", func(t *testing.T) {
		broken := map[string]string{}
		for k, v := range valid {
			broken[k] = v
		}
		delete(broken, override.AnnotationExpiresAt)
		assert.Nil(t, activeOverride(newVpa(broken), now), "an override without an expiry must not apply")
	})
}

func TestPinnedResources(t *testing.T) {
	ov := pin(t, map[string]corev1.ResourceList{"app": quantities("700m", "300Mi")})

	got := pinnedResources(ov, containers("app", "sidecar"))
	require.Len(t, got, 1)

	app := got["app"]
	assert.Equal(t, model.ResourceAmount(700), app.Target[model.ResourceCPU])
	assert.Equal(t, model.ResourceAmount(300<<20), app.Target[model.ResourceMemory])
	assert.Equal(t, app.Target, app.LowerBound, "pinned bounds equal the target so pods that match are left alone")
	assert.Equal(t, app.Target, app.UpperBound)

	assert.Empty(t, pinnedResources(ov, containers("sidecar")), "a container the VPA does not manage is ignored")
	assert.Empty(t, pinnedResources(nil, containers("app")))

	paused := &override.Override{Mode: override.ModePaused, SetBy: "bob", SetAt: now, ExpiresAt: expires}
	assert.Empty(t, pinnedResources(paused, containers("app")))
}

func TestResolveResources(t *testing.T) {
	ov := pin(t, map[string]corev1.ResourceList{"app": quantities("700m", "300Mi")})

	t.Run("auto calculates everything", func(t *testing.T) {
		calc := &calculatedRecommender{}
		r := &recommender{podResourceRecommender: calc}

		got, err := r.resolveResources(newVpa(nil), containers("app", "sidecar"), nil)
		require.NoError(t, err)
		assert.Equal(t, 1, calc.calls)
		assert.Equal(t, model.ResourceAmount(100), got["app"].Target[model.ResourceCPU])
		assert.Equal(t, model.ResourceAmount(100), got["sidecar"].Target[model.ResourceCPU])
	})

	t.Run("everything pinned skips the calculation", func(t *testing.T) {
		calc := &calculatedRecommender{}
		r := &recommender{podResourceRecommender: calc}

		got, err := r.resolveResources(newVpa(nil), containers("app"), ov)
		require.NoError(t, err)
		assert.Equal(t, 0, calc.calls, "a pin must not depend on the calculation, or on VictoriaMetrics being up")
		assert.Equal(t, model.ResourceAmount(700), got["app"].Target[model.ResourceCPU])
	})

	t.Run("partly pinned mixes pinned and calculated", func(t *testing.T) {
		calc := &calculatedRecommender{}
		r := &recommender{podResourceRecommender: calc}

		got, err := r.resolveResources(newVpa(nil), containers("app", "sidecar"), ov)
		require.NoError(t, err)
		assert.Equal(t, 1, calc.calls)
		assert.Equal(t, model.ResourceAmount(700), got["app"].Target[model.ResourceCPU], "pinned")
		assert.Equal(t, model.ResourceAmount(100), got["sidecar"].Target[model.ResourceCPU], "calculated")
	})

	t.Run("pin for an unmanaged container changes nothing", func(t *testing.T) {
		calc := &calculatedRecommender{}
		r := &recommender{podResourceRecommender: calc}

		got, err := r.resolveResources(newVpa(nil), containers("sidecar"), ov)
		require.NoError(t, err)
		assert.Equal(t, 1, calc.calls)
		assert.Equal(t, model.ResourceAmount(100), got["sidecar"].Target[model.ResourceCPU])
		assert.NotContains(t, got, "app")
	})
}
