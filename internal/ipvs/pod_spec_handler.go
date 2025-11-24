package ipvs

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PodSpecHandler handles Pod specification adjustments
type PodSpecHandler struct {
	client.Client
}

// NewPodSpecHandler creates a new PodSpecHandler instance
func NewPodSpecHandler(client client.Client) *PodSpecHandler {
	return &PodSpecHandler{
		Client: client,
	}
}

// Constants for minimum resource values
const MinMilli = int64(10)

// ===================== Pod specification adjustment functions =====================

// DegradePodRequests: Reduces Pod CPU request amount by a specified ratio
func (h *PodSpecHandler) DegradePodRequests(ctx context.Context, pod *corev1.Pod, ratio float64) error {
	logger := log.Log.WithValues("McKube/rt.degradation", "DegradePodRequests")
	logger.V(0).Info("Starting degradation",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"ratio", ratio)
	type containerPatch struct {
		Name      string `json:"name"`
		Resources struct {
			Requests map[string]string `json:"requests,omitempty"`
			Limits   map[string]string `json:"limits,omitempty"`
		} `json:"resources"`
	}
	type patchSpec struct {
		Containers []containerPatch `json:"containers"`
	}
	type patchRoot struct {
		Spec patchSpec `json:"spec"`
	}

	var pr patchRoot
	for _, c := range pod.Spec.Containers {
		// Only patch containers that have CPU requests
		if c.Resources.Requests == nil {
			continue
		}
		reqCPU := c.Resources.Requests.Cpu()
		if reqCPU == nil || reqCPU.IsZero() {
			continue
		}

		oldMilli := reqCPU.MilliValue()
		newMilli := int64(float64(oldMilli) * (1.0 - ratio))
		if newMilli < MinMilli {
			newMilli = MinMilli
		}

		logger.V(0).Info("Processing container for degradation",
			"container", c.Name,
			"oldMilli", oldMilli,
			"newMilli", newMilli,
			"ratio", ratio)

		entry := containerPatch{Name: c.Name}

		entry.Resources.Requests = map[string]string{
			string(corev1.ResourceCPU): fmt.Sprintf("%dm", newMilli),
		}

		if len(c.Resources.Limits) > 0 {
			entry.Resources.Limits = make(map[string]string)
			for resName, qty := range c.Resources.Limits {
				entry.Resources.Limits[string(resName)] = qty.String()
			}
		}
		pr.Spec.Containers = append(pr.Spec.Containers, entry)
	}

	if len(pr.Spec.Containers) == 0 {
		logger.V(0).Info("No containers to degrade, skipping")
		return nil
	}

	patchBytes, err := json.Marshal(pr)
	if err != nil {
		logger.Error(err, "Failed to marshal patch JSON")
		return err
	}

	if err := h.SubResource("resize").Patch(ctx, pod, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
		logger.Error(err, "Resize patch failed")
		return err
	}

	logger.V(0).Info("Degradation completed successfully")
	return nil
}

// PickHighestCPUFromMilli: Selects the Pod with the highest CPU request amount
func PickHighestCPUFromMilli(pods []*corev1.Pod, podMilli map[string]int64) *corev1.Pod {
	var best *corev1.Pod
	var bestMilli int64 = -1
	for _, p := range pods {
		if m, ok := podMilli[p.Name]; ok {
			if m > bestMilli {
				bestMilli = m
				best = p
			}
		}
	}
	return best
}

// FilterPodsByCriticality: Filters Pods with a specific Criticality value
// Used later to group Pods with the same Criticality for processing in processCurrentTier()
func FilterPodsByCriticality(pods []*corev1.Pod, rtData map[string]RealTimeData, crit string) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range pods {
		app := p.Labels["sdv.com"]
		if app == "" {
			continue
		}
		if rt, ok := rtData[app]; ok {
			if rt.Criticality == crit {
				out = append(out, p)
			}
		}
	}
	return out
}

// HasActionableInTier: Checks if there are actionable Pods for a specific Criticality tier
func HasActionableInTier(pods []*corev1.Pod, rtData map[string]RealTimeData, tier string) bool {
	targets := FilterPodsByCriticality(pods, rtData, tier)
	if len(targets) == 0 {
		return false
	}
	for _, p := range targets {
		for _, c := range p.Spec.Containers {
			if c.Resources.Requests == nil || c.Resources.Requests.Cpu() == nil {
				continue
			}
			if c.Resources.Requests.Cpu().MilliValue() > MinMilli {
				return true
			}
		}
	}
	return false
}
