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
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/logic"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/override"
)

// activeOverride returns the manual override that applies to the VPA at the given
// time, or nil when the VPA is in Auto. An expired override is Auto again, and so
// is a malformed one: an override that cannot be read (in particular one without
// an expiry) must not turn into a forgotten exception.
func activeOverride(vpa *model.Vpa, now time.Time) *override.Override {
	ov, err := override.FromAnnotations(vpa.Annotations)
	if err != nil {
		klog.ErrorS(err, "Ignoring malformed VPA override, using the calculated recommendation", "vpa", klog.KRef(vpa.ID.Namespace, vpa.ID.VpaName))
		return nil
	}
	if ov == nil {
		return nil
	}
	if ov.Expired(now) {
		klog.V(4).InfoS("VPA override expired, using the calculated recommendation", "vpa", klog.KRef(vpa.ID.Namespace, vpa.ID.VpaName), "setBy", ov.SetBy, "expiredAt", ov.ExpiresAt)
		return nil
	}
	return ov
}

// pinnedResources turns a Pinned override into recommendations for the containers
// the VPA actually manages. Target, lower and upper bound are all the pinned value:
// a pod whose request differs from it is outside the recommended range, so the
// updater picks it up at once and leaves alone the pods that already match.
func pinnedResources(ov *override.Override, containers model.ContainerNameToAggregateStateMap) logic.RecommendedPodResources {
	pinned := make(logic.RecommendedPodResources)
	if ov == nil || ov.Mode != override.ModePinned {
		return pinned
	}
	for name := range containers {
		cpu, memory := ov.Quantities(name)
		if cpu == nil || memory == nil {
			continue
		}
		resources := model.Resources{
			model.ResourceCPU:    model.ResourceAmount(cpu.MilliValue()),
			model.ResourceMemory: model.ResourceAmount(memory.Value()),
		}
		pinned[name] = logic.RecommendedContainerResources{Target: resources, LowerBound: resources, UpperBound: resources}
	}
	return pinned
}

// resolveResources returns the recommendation for the VPA: the calculated one, with
// the pinned containers replaced by the pinned values. When every container is
// pinned nothing is calculated, so a pin keeps working while VictoriaMetrics is down.
func (r *recommender) resolveResources(vpa *model.Vpa, containers model.ContainerNameToAggregateStateMap, ov *override.Override) (logic.RecommendedPodResources, error) {
	pinned := pinnedResources(ov, containers)
	if len(pinned) > 0 && len(pinned) == len(containers) {
		return pinned, nil
	}

	var resources logic.RecommendedPodResources
	if r.vmRecommender != nil {
		var err error
		resources, err = recommendFromVictoriaMetrics(r.vmRecommender, vpa, containers)
		if err != nil {
			return nil, fmt.Errorf("cannot get recommendation from VictoriaMetrics: %w", err)
		}
	} else {
		resources = r.podResourceRecommender.GetRecommendedPodResources(containers)
	}

	for name, p := range pinned {
		resources[name] = p
	}
	return resources, nil
}
