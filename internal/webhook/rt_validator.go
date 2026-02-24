package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	mcoperatorv1 "mc-kube/api/v1"
	"mc-kube/internal/cpupool"
)

// +kubebuilder:webhook:path=/validate-rt-pod,mutating=false,failurePolicy=fail,sideEffects=NoneOnDryRun,groups="",resources=pods,verbs=create,versions=v1,name=vrtpod.kb.io,admissionReviewVersions=v1

type RTValidator struct {
	Client client.Client
}

func (v *RTValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Find the matching McKube CR.
	mckubeCR, err := v.findMcKubeForPod(ctx, pod)
	if err != nil {
		log.Log.Error(err, "Failed to find McKube CR for pod")
		return admission.Allowed("No McKube CR found, allowing pod creation")
	}
	if mckubeCR == nil || mckubeCR.Spec.RTSettings == nil {
		return admission.Allowed("No RT settings configured, allowing pod creation")
	}

	rtSettings := mckubeCR.Spec.RTSettings

	// ── Phase 1: mathematical parameter validation (unchanged) ───────────────
	if err := v.validateRTSettings(rtSettings); err != nil {
		return admission.Denied(fmt.Sprintf("Invalid RT settings: %v", err))
	}

	// ── Phase 2: RT CPU budget feasibility check ──────────────────────────────
	// Fetch the node list so SelectBestNodeAndCore knows CPU core counts.
	nodeList := &corev1.NodeList{}
	if err := v.Client.List(ctx, nodeList); err != nil {
		log.Log.Error(err, "Failed to list nodes; skipping feasibility check")
		return admission.Allowed("RT settings are valid (feasibility check skipped due to error)")
	}

	requiredBudget := float64(rtSettings.RuntimeHi) / float64(rtSettings.Period)
	criticality := mckubeCR.Spec.Criticality

	// Determine the core that Mutating Webhook selected (if any).
	// Priority: Status.AllocatedCore > Spec.RTSettings.Core (already patched by Mutating).
	selectedCoreStr := ""
	if mckubeCR.Status.AllocatedCore != "" {
		selectedCoreStr = mckubeCR.Status.AllocatedCore
	} else if rtSettings.Core != nil {
		selectedCoreStr = *rtSettings.Core
	}
	selectedNode := mckubeCR.Spec.Node // may be empty if pod not yet bound

	if selectedCoreStr != "" && selectedNode != "" {
		// ── Case A: Mutating Webhook has already fixed the node+core.
		// Validate feasibility against that specific (node, core) pair.
		coreIDs := cpupool.ParseCoreSet(selectedCoreStr)
		if len(coreIDs) == 0 {
			return admission.Denied(fmt.Sprintf("Invalid core specification: %q", selectedCoreStr))
		}

		feasible, victims := cpupool.CheckNodeCoreFeasibility(selectedNode, coreIDs[0], requiredBudget, criticality)
		if !feasible {
			return admission.Denied(fmt.Sprintf(
				"Insufficient RT CPU budget on node %q core %d for pod %q (criticality=%s, requiredBudget=%.3f)",
				selectedNode, coreIDs[0], pod.Name, criticality, requiredBudget))
		}

		if len(victims) > 0 {
			log.Log.V(0).Info("Pod admitted; lower-criticality pods will be evicted by Controller",
				"pod", pod.Name, "node", selectedNode, "core", coreIDs[0], "victims", victims)
		}
		return admission.Allowed("RT settings valid and node/core feasibility confirmed")
	}

	// ── Case B: Core not yet determined (Mutating failed or core still unset).
	// Run the full Worst-Fit + eviction-lookahead search over all nodes.
	log.Log.V(0).Info("Core not fixed by Mutating Webhook; running full feasibility search",
		"pod", pod.Name)

	result := cpupool.SelectBestNodeAndCore(nodeList.Items, requiredBudget, criticality, nil)
	if !result.Feasible {
		return admission.Denied(fmt.Sprintf(
			"No node in the cluster has sufficient RT CPU budget for pod %q (criticality=%s, requiredBudget=%.3f)",
			pod.Name, criticality, requiredBudget))
	}

	log.Log.V(0).Info("Pod admitted via full-cluster feasibility search",
		"pod", pod.Name,
		"suggestedNode", result.SelectedNodeName,
		"suggestedCore", result.SelectedCoreID,
		"victims", result.VictimPodNames)

	return admission.Allowed("RT settings valid and cluster-wide feasibility confirmed")
}

// findMcKubeForPod returns the McKube CR matching the pod, or nil if none.
func (v *RTValidator) findMcKubeForPod(ctx context.Context, pod *corev1.Pod) (*mcoperatorv1.McKube, error) {
	mckubeList := &mcoperatorv1.McKubeList{}
	if err := v.Client.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}
	for i := range mckubeList.Items {
		mc := &mckubeList.Items[i]
		if mc.Spec.PodName == pod.Name && mc.Spec.RTSettings != nil {
			return mc, nil
		}
	}
	return nil, nil
}

// validateRTSettings validates RT configuration parameters (mathematical checks).
func (v *RTValidator) validateRTSettings(rtSettings *mcoperatorv1.RTSettings) error {
	if rtSettings.Period <= 0 {
		return fmt.Errorf("RT period must be positive, got: %d", rtSettings.Period)
	}
	if rtSettings.RuntimeLow <= 0 {
		return fmt.Errorf("RT runtime_low must be positive, got: %d", rtSettings.RuntimeLow)
	}
	if rtSettings.RuntimeLow > rtSettings.Period {
		return fmt.Errorf("RT runtime_low (%d) cannot be greater than period (%d)", rtSettings.RuntimeLow, rtSettings.Period)
	}
	if rtSettings.RuntimeHi <= 0 {
		return fmt.Errorf("RT runtime_hi must be positive, got: %d", rtSettings.RuntimeHi)
	}
	if rtSettings.RuntimeHi > rtSettings.Period {
		return fmt.Errorf("RT runtime_hi (%d) cannot be greater than period (%d)", rtSettings.RuntimeHi, rtSettings.Period)
	}
	if rtSettings.RuntimeLow > rtSettings.RuntimeHi {
		return fmt.Errorf("RT runtime_low (%d) cannot be greater than runtime_hi (%d)", rtSettings.RuntimeLow, rtSettings.RuntimeHi)
	}
	maxRuntime := int(float64(rtSettings.Period) * cpupool.RTCapacityPerCore)
	if rtSettings.RuntimeHi > maxRuntime {
		return fmt.Errorf("RT runtime_hi (%d) exceeds %.0f%% of period (%d), max allowed: %d",
			rtSettings.RuntimeHi, cpupool.RTCapacityPerCore*100, rtSettings.Period, maxRuntime)
	}
	if rtSettings.Core != nil && len(*rtSettings.Core) == 0 {
		return fmt.Errorf("RT core setting cannot be empty string")
	}
	return nil
}
