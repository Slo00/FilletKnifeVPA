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

// Package override defines a manual override of VPA recommendations stored as
// annotations on the VerticalPodAutoscaler object itself. The recommender reads
// it on every cycle (it rewrites status.recommendation each time, so an override
// has to be applied inside it, not on top of it); the web UI writes it.
// See docs/adr/0003-manual-overrides.md.
package override

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Mode is the override mode. The absence of an override means Auto.
type Mode string

const (
	// ModePinned makes the recommender publish the given values instead of computing them.
	ModePinned Mode = "Pinned"
	// ModePaused makes the recommender skip the VPA, leaving status.recommendation as is.
	ModePaused Mode = "Paused"
)

const (
	prefix = "vpa-override.kubernetes.io/"

	// AnnotationMode is the switch: without it the rest of the annotations is ignored.
	AnnotationMode = prefix + "mode"
	// AnnotationResources is a JSON object: container name -> ResourceList (cpu, memory).
	AnnotationResources = prefix + "resources"
	// AnnotationSetBy is who set the override.
	AnnotationSetBy = prefix + "set-by"
	// AnnotationSetAt is when the override was set, RFC 3339.
	AnnotationSetAt = prefix + "set-at"
	// AnnotationExpiresAt is when the override stops being applied, RFC 3339. Mandatory:
	// an override without an expiry turns into a forgotten exception.
	AnnotationExpiresAt = prefix + "expires-at"
	// AnnotationReason is a free-form explanation.
	AnnotationReason = prefix + "reason"
)

var allAnnotations = []string{
	AnnotationMode, AnnotationResources, AnnotationSetBy,
	AnnotationSetAt, AnnotationExpiresAt, AnnotationReason,
}

// AnnotationKeys returns every annotation key owned by an override. Removing all
// of them returns the VPA to Auto.
func AnnotationKeys() []string {
	return append([]string(nil), allAnnotations...)
}

// Override is a parsed manual override.
type Override struct {
	Mode Mode
	// Resources is only set for ModePinned: container name -> pinned requests.
	Resources map[string]corev1.ResourceList
	SetBy     string
	SetAt     time.Time
	ExpiresAt time.Time
	Reason    string
}

// Expired reports whether the override no longer applies at the given time.
func (o *Override) Expired(now time.Time) bool {
	return !now.Before(o.ExpiresAt)
}

// Validate checks the override is well-formed. It does not check policy such as a
// maximum lifetime: that belongs to whoever writes overrides.
func (o *Override) Validate() error {
	switch o.Mode {
	case ModePinned, ModePaused:
	default:
		return fmt.Errorf("unknown mode %q, want %q or %q", o.Mode, ModePinned, ModePaused)
	}
	if strings.TrimSpace(o.SetBy) == "" {
		return fmt.Errorf("set-by is empty")
	}
	if o.SetAt.IsZero() {
		return fmt.Errorf("set-at is empty")
	}
	if o.ExpiresAt.IsZero() {
		return fmt.Errorf("expires-at is empty: an override must have an expiry")
	}
	if !o.ExpiresAt.After(o.SetAt) {
		return fmt.Errorf("expires-at %s is not after set-at %s", o.ExpiresAt.Format(time.RFC3339), o.SetAt.Format(time.RFC3339))
	}

	if o.Mode == ModePaused {
		if len(o.Resources) > 0 {
			return fmt.Errorf("mode %s takes no resources", ModePaused)
		}
		return nil
	}

	if len(o.Resources) == 0 {
		return fmt.Errorf("mode %s needs resources for at least one container", ModePinned)
	}
	for name, list := range o.Resources {
		if name == "" {
			return fmt.Errorf("empty container name")
		}
		for res := range list {
			if res != corev1.ResourceCPU && res != corev1.ResourceMemory {
				return fmt.Errorf("container %q: unsupported resource %q, only cpu and memory", name, res)
			}
		}
		// Both are required: a recommendation with only one of them would silently
		// leave the other resource unmanaged.
		for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			q, ok := list[res]
			if !ok {
				return fmt.Errorf("container %q: %s is missing, cpu and memory are both required", name, res)
			}
			if q.Sign() <= 0 {
				return fmt.Errorf("container %q: %s must be positive, got %s", name, res, q.String())
			}
		}
	}
	return nil
}

// ToAnnotations validates the override and returns the annotations to write.
func (o *Override) ToAnnotations() (map[string]string, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	annotations := map[string]string{
		AnnotationMode:      string(o.Mode),
		AnnotationSetBy:     o.SetBy,
		AnnotationSetAt:     o.SetAt.UTC().Format(time.RFC3339),
		AnnotationExpiresAt: o.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if o.Reason != "" {
		annotations[AnnotationReason] = o.Reason
	}
	if o.Mode == ModePinned {
		raw, err := json.Marshal(o.Resources)
		if err != nil {
			return nil, fmt.Errorf("marshal resources: %w", err)
		}
		annotations[AnnotationResources] = string(raw)
	}
	return annotations, nil
}

// FromAnnotations parses an override. It returns (nil, nil) when there is none,
// which is the case whenever the mode annotation is absent. A present but
// malformed override is an error, so the caller can report it rather than guess.
// Expired overrides are returned as is: check Expired.
func FromAnnotations(annotations map[string]string) (*Override, error) {
	mode := strings.TrimSpace(annotations[AnnotationMode])
	if mode == "" {
		return nil, nil
	}

	o := &Override{
		Mode:   Mode(mode),
		SetBy:  annotations[AnnotationSetBy],
		Reason: annotations[AnnotationReason],
	}

	var err error
	if o.SetAt, err = parseTime(annotations, AnnotationSetAt); err != nil {
		return nil, err
	}
	if o.ExpiresAt, err = parseTime(annotations, AnnotationExpiresAt); err != nil {
		return nil, err
	}

	if raw := annotations[AnnotationResources]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &o.Resources); err != nil {
			return nil, fmt.Errorf("annotation %s: %w", AnnotationResources, err)
		}
	}

	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("invalid override: %w", err)
	}
	return o, nil
}

func parseTime(annotations map[string]string, key string) (time.Time, error) {
	raw := annotations[key]
	if raw == "" {
		return time.Time{}, fmt.Errorf("invalid override: annotation %s is missing", key)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid override: annotation %s: %w", key, err)
	}
	return t, nil
}

// Quantities returns the pinned requests of a container as plain quantities.
func (o *Override) Quantities(container string) (cpu, memory *resource.Quantity) {
	list, ok := o.Resources[container]
	if !ok {
		return nil, nil
	}
	if q, ok := list[corev1.ResourceCPU]; ok {
		cpu = &q
	}
	if q, ok := list[corev1.ResourceMemory]; ok {
		memory = &q
	}
	return cpu, memory
}
