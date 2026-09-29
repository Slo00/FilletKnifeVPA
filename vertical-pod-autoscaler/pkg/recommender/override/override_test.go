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

package override

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var (
	t0 = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(72 * time.Hour)
)

func pinned() *Override {
	return &Override{
		Mode: ModePinned,
		Resources: map[string]corev1.ResourceList{
			"hamster": {
				corev1.ResourceCPU:    resource.MustParse("700m"),
				corev1.ResourceMemory: resource.MustParse("300Mi"),
			},
		},
		SetBy:     "alice",
		SetAt:     t0,
		ExpiresAt: t1,
		Reason:    "release freeze",
	}
}

func TestRoundTrip(t *testing.T) {
	annotations, err := pinned().ToAnnotations()
	require.NoError(t, err)

	got, err := FromAnnotations(annotations)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, ModePinned, got.Mode)
	assert.Equal(t, "alice", got.SetBy)
	assert.Equal(t, "release freeze", got.Reason)
	assert.True(t, t0.Equal(got.SetAt))
	assert.True(t, t1.Equal(got.ExpiresAt))

	cpu, memory := got.Quantities("hamster")
	require.NotNil(t, cpu)
	require.NotNil(t, memory)
	assert.Equal(t, int64(700), cpu.MilliValue())
	assert.Equal(t, int64(300*1024*1024), memory.Value())

	cpu, memory = got.Quantities("other")
	assert.Nil(t, cpu)
	assert.Nil(t, memory)
}

func TestPausedRoundTrip(t *testing.T) {
	o := &Override{Mode: ModePaused, SetBy: "bob", SetAt: t0, ExpiresAt: t1}
	annotations, err := o.ToAnnotations()
	require.NoError(t, err)
	assert.NotContains(t, annotations, AnnotationResources)

	got, err := FromAnnotations(annotations)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, ModePaused, got.Mode)
}

func TestNoOverride(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"nil":                nil,
		"empty":              {},
		"unrelated":          {"foo": "bar"},
		"leftovers, no mode": {AnnotationSetBy: "alice", AnnotationExpiresAt: t1.Format(time.RFC3339)},
		"blank mode":         {AnnotationMode: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := FromAnnotations(annotations)
			assert.NoError(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestExpired(t *testing.T) {
	o := pinned()
	assert.False(t, o.Expired(t0))
	assert.False(t, o.Expired(t1.Add(-time.Second)))
	assert.True(t, o.Expired(t1), "the expiry instant itself is already expired")
	assert.True(t, o.Expired(t1.Add(time.Hour)))
}

func TestFromAnnotationsRejectsMalformed(t *testing.T) {
	valid, err := pinned().ToAnnotations()
	require.NoError(t, err)

	mutate := func(edit func(map[string]string)) map[string]string {
		a := map[string]string{}
		for k, v := range valid {
			a[k] = v
		}
		edit(a)
		return a
	}

	for name, annotations := range map[string]map[string]string{
		"unknown mode":        mutate(func(a map[string]string) { a[AnnotationMode] = "Fast" }),
		"no expiry":           mutate(func(a map[string]string) { delete(a, AnnotationExpiresAt) }),
		"bad expiry":          mutate(func(a map[string]string) { a[AnnotationExpiresAt] = "tomorrow" }),
		"no set-at":           mutate(func(a map[string]string) { delete(a, AnnotationSetAt) }),
		"no set-by":           mutate(func(a map[string]string) { delete(a, AnnotationSetBy) }),
		"expiry before start": mutate(func(a map[string]string) { a[AnnotationExpiresAt] = t0.Add(-time.Hour).Format(time.RFC3339) }),
		"pinned no resources": mutate(func(a map[string]string) { delete(a, AnnotationResources) }),
		"bad json":            mutate(func(a map[string]string) { a[AnnotationResources] = "{" }),
		"bad quantity":        mutate(func(a map[string]string) { a[AnnotationResources] = `{"hamster":{"cpu":"lots"}}` }),
		"zero cpu":            mutate(func(a map[string]string) { a[AnnotationResources] = `{"hamster":{"cpu":"0","memory":"300Mi"}}` }),
		"negative memory":     mutate(func(a map[string]string) { a[AnnotationResources] = `{"hamster":{"cpu":"700m","memory":"-1Mi"}}` }),
		"unsupported": mutate(func(a map[string]string) {
			a[AnnotationResources] = `{"hamster":{"cpu":"700m","memory":"300Mi","ephemeral-storage":"1Gi"}}`
		}),
		"empty container": mutate(func(a map[string]string) { a[AnnotationResources] = `{"hamster":{}}` }),
		"memory missing":  mutate(func(a map[string]string) { a[AnnotationResources] = `{"hamster":{"cpu":"700m"}}` }),
		"cpu missing":     mutate(func(a map[string]string) { a[AnnotationResources] = `{"hamster":{"memory":"300Mi"}}` }),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := FromAnnotations(annotations)
			assert.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestPausedWithResourcesIsInvalid(t *testing.T) {
	o := pinned()
	o.Mode = ModePaused
	_, err := o.ToAnnotations()
	assert.Error(t, err)
}

func TestToAnnotationsRefusesInvalid(t *testing.T) {
	o := pinned()
	o.ExpiresAt = time.Time{}
	_, err := o.ToAnnotations()
	assert.Error(t, err, "an override without an expiry must not be writable")
}

func TestAnnotationKeysCoversEveryWrittenKey(t *testing.T) {
	annotations, err := pinned().ToAnnotations()
	require.NoError(t, err)
	keys := AnnotationKeys()
	for k := range annotations {
		assert.Contains(t, keys, k, "clearing an override must remove %s", k)
	}
}
