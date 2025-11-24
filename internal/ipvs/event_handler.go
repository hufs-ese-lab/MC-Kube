package ipvs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// EventHandler handles CPU pressure and recovery events
type EventHandler struct {
	client.Client
	DataCollector  *DataCollector
	PodSpecHandler *PodSpecHandler
}

// NodePressureState represents the pressure state of a node
type NodePressureState struct {
	AboveSec       int
	Tiers          []string
	CurrentTierIdx int
	CurrentTier    string
	PerTier        map[string]*PerTierState
}

// PerTierState represents the state for each criticality tier
type PerTierState struct {
	ElapsedSec      int
	DegradeCount    int // Number of degradation attempts
	EvictTried      bool
	MissingTicks    int
	LastDegradeTime int // Timestamp of last degradation attempt
}

// Constants for event handling
const (
	AnnUsageKey   = "mckube.sdv.com/cpu-usage"
	AnnDurKey     = "mckube.sdv.com/cpu-over90-duration-s"
	AnnCpuBusyKey = "mckube.sdv.com/isCpuBusy"

	TargetNamespace      = "default"
	TierMissingTolerance = 2
)

// Global state management
var (
	PressureState   = make(map[string]*NodePressureState)
	ProcessingNodes = make(map[string]bool)
	ProcessingMutex sync.RWMutex

	// Criticality ranking
	CriticalityRank = map[string]int{
		"Low":    0,
		"Middle": 1,
		"High":   2,
	}
)

// NewEventHandler creates a new EventHandler instance
func NewEventHandler(client client.Client, dataCollector *DataCollector, podSpecHandler *PodSpecHandler) *EventHandler {
	return &EventHandler{
		Client:         client,
		DataCollector:  dataCollector,
		PodSpecHandler: podSpecHandler,
	}
}

// ===================== Event-based processing functions =====================

// HandleNodeCPUPressure: Parses annotation and CR information when node CPU usage exceeds 90% and passes to processCurrentTier()
func (e *EventHandler) HandleNodeCPUPressure(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPUPressureHandler", "EventDriven", "node", nodeName)

	node := &corev1.Node{}
	if err := e.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		logger.Error(err, "Failed to get node")
		return
	}

	ann := node.GetAnnotations()
	if ann == nil {
		return
	}

	cpuUsageStr := strings.TrimSpace(ann[AnnUsageKey])
	if cpuUsageStr == "" {
		logger.Error(fmt.Errorf("missing annotation %q", AnnUsageKey), "Failed to get CPU usage from node")
		return
	}

	cpuUsage, err := strconv.ParseFloat(cpuUsageStr, 64)
	if err != nil {
		logger.Error(err, "Failed to parse CPU usage from node annotation", "value", cpuUsageStr)
		return
	}

	overSecStr := strings.TrimSpace(ann[AnnDurKey])
	overSec := 0
	if overSecStr != "" {
		if sec, err := strconv.ParseInt(overSecStr, 10, 64); err == nil {
			if sec >= 0 {
				overSec = int(sec)
			}
		} else {
			logger.V(1).Info("Failed to parse over90 duration; treating as 0", "err", err.Error(), "value", overSecStr)
		}
	}

	rtData, err := e.DataCollector.GetRealTimeData(ctx)
	if err != nil {
		logger.Error(err, "Failed to get RT data")
		return
	}

	podList := &corev1.PodList{}
	if err := e.List(ctx, podList, client.InNamespace(TargetNamespace)); err != nil {
		logger.Error(err, "Failed to list pods", "namespace", TargetNamespace)
		return
	}

	var nodePods []*corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Spec.NodeName == nodeName {
			nodePods = append(nodePods, p)
		}
	}

	if len(nodePods) == 0 {
		return
	}

	podMilli, err := e.DataCollector.ListPodMilliCPUByNode(ctx, TargetNamespace, node)
	if err != nil {
		logger.Error(err, "Failed to list pod milliCPU (requests-based)")
		return
	}

	state := PressureState[nodeName]
	if state == nil {
		state = &NodePressureState{
			AboveSec:       0,
			Tiers:          nil,
			CurrentTierIdx: 0,
			CurrentTier:    "",
			PerTier:        map[string]*PerTierState{},
		}
		PressureState[nodeName] = state
	}
	state.Tiers = CollectSortedTiers(nodePods, rtData)
	state.AboveSec = overSec

	ReindexCurrentTier(state)

	// Skip if no tiers
	if len(state.Tiers) == 0 {
		return
	}

	// Set current tier (safe initialization if still empty)
	if state.CurrentTier == "" || state.CurrentTierIdx >= len(state.Tiers) {
		state.CurrentTierIdx = 0
		state.CurrentTier = state.Tiers[0]
		if _, ok := state.PerTier[state.CurrentTier]; !ok {
			state.PerTier[state.CurrentTier] = &PerTierState{}
		}
	}

	// Process Pods that can be acted upon for current Criticality
	e.ProcessCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
}

// ProcessCurrentTier: Performs the following logic for the current Criticality tier
//
// 1. When CPU usage exceeds 90% for 1+ seconds:
//   - Apply 20% reduction to CPU requests of the Pod with the lowest Criticality among currently deployed Pods (Graceful degradation)
//
// 2. When CPU usage exceeds 90% for 10+ seconds:
//   - Evict the Pod that previously had Degradation applied
func (e *EventHandler) ProcessCurrentTier(ctx context.Context, logger logr.Logger, state *NodePressureState, nodePods []*corev1.Pod, rtData map[string]RealTimeData, podMilli map[string]int64, overSec int, cpuUsage float64) {
	curTier := state.CurrentTier
	pts := state.PerTier[curTier]
	if pts == nil {
		pts = &PerTierState{}
		state.PerTier[curTier] = pts
	}

	targets := FilterPodsByCriticality(nodePods, rtData, curTier)
	targetsCount := len(targets)

	logger.V(0).Info("Node pressure snapshot",
		"cpu(%)", fmt.Sprintf("%.1f", cpuUsage),
		"over90Sec", overSec,
		"currentTier", curTier,
		"targetsCount", targetsCount,
		"degradeCount", pts.DegradeCount,
		"evictTried", pts.EvictTried,
	)

	// If no Pods to handle in the same tier, escalate to next tier
	if targetsCount == 0 {
		pts.MissingTicks++

		if pts.MissingTicks >= TierMissingTolerance {
			if nextTier, ok := NextHigherTier(curTier, state.Tiers); ok {
				state.CurrentTier = nextTier

				for i, t := range state.Tiers {
					if t == nextTier {
						state.CurrentTierIdx = i
						break
					}
				}
				if _, ok := state.PerTier[nextTier]; !ok {
					state.PerTier[nextTier] = &PerTierState{}
				}
				// Recursively process next tier
				e.ProcessCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
			}
		}
		return
	}

	pts.MissingTicks = 0

	// If CPU threshold duration is 1+ seconds, perform graceful degradation repeatedly
	if overSec >= 1 && !pts.EvictTried {
		// Perform degradation every 3 seconds or on first attempt
		shouldDegrade := pts.DegradeCount == 0 || (overSec-pts.LastDegradeTime >= 3)

		if shouldDegrade {
			top := PickHighestCPUFromMilli(targets, podMilli)
			if top != nil {
				// Check current CPU request amount
				currentMilli := int64(0)
				if podMilli[top.Name] > 0 {
					currentMilli = podMilli[top.Name]
				}

				// Do not degrade below minMilli
				if currentMilli > MinMilli {
					degradeRatio := 0.2 // 20% (Graceful degradation ratio)
					if err := e.PodSpecHandler.DegradePodRequests(ctx, top, degradeRatio); err != nil {
						logger.Error(err, "Graceful degradation failed",
							"tier", curTier, "pod", top.Name, "attempt", pts.DegradeCount+1)
					} else {
						pts.DegradeCount++
						pts.LastDegradeTime = overSec
					}
				} else {
					pts.DegradeCount = 999 // Further degradation impossible
				}
			}
		}
	}

	// If 90% duration is 10+ seconds, perform eviction
	if overSec >= 10 && pts.DegradeCount > 0 && !pts.EvictTried {
		victim := PickHighestCPUFromMilli(targets, podMilli)
		if victim != nil {
			if err := e.EvictPod(ctx, victim); err != nil {
				logger.Error(err, "Eviction failed",
					"tier", curTier, "pod", victim.Name)
				pts.EvictTried = true
			} else {
				pts.EvictTried = true
			}
		}
	}

	// After processing complete, escalate to next tier if no more actionable Pods
	if pts.DegradeCount > 0 && pts.EvictTried {
		actionable := HasActionableInTier(nodePods, rtData, curTier)
		if !actionable {
			if nextTier, ok := NextHigherTier(curTier, state.Tiers); ok {
				state.CurrentTier = nextTier

				for i, t := range state.Tiers {
					if t == nextTier {
						state.CurrentTierIdx = i
						break
					}
				}
				if _, ok := state.PerTier[nextTier]; !ok {
					state.PerTier[nextTier] = &PerTierState{}
				}
			}
		}
	}
}

// FindObjectsForNode: Event handler function that detects node annotation changes
func (e *EventHandler) FindObjectsForNode(ctx context.Context, node client.Object) []reconcile.Request {
	nodeObj := node.(*corev1.Node)

	ann := nodeObj.GetAnnotations()
	if ann == nil {
		return []reconcile.Request{}
	}

	cpuUsageStr := strings.TrimSpace(ann[AnnUsageKey])
	if cpuUsageStr == "" {
		return []reconcile.Request{}
	}
	cpuUsage, err := strconv.ParseFloat(cpuUsageStr, 64)
	if err != nil {
		return []reconcile.Request{}
	}

	// Check isCpuBusy status
	isCpuBusyStr := strings.TrimSpace(ann[AnnCpuBusyKey])
	isCpuBusy := true // Default value true
	if isCpuBusyStr != "" {
		if busy, err := strconv.ParseBool(isCpuBusyStr); err == nil {
			isCpuBusy = busy
		}
	}

	// Skip if node is already being processed
	ProcessingMutex.Lock()
	if ProcessingNodes[nodeObj.Name] {
		ProcessingMutex.Unlock()
		return []reconcile.Request{}
	}
	ProcessingNodes[nodeObj.Name] = true
	ProcessingMutex.Unlock()

	logger := log.Log.WithValues("McKube/rt.NodeEvent", "CPU-Event")
	logger.V(0).Info("CPU annotation change detected",
		"node", nodeObj.Name,
		"cpu(%)", fmt.Sprintf("%.1f", cpuUsage),
		"isCpuBusy", isCpuBusy)

	// Process CPU pressure asynchronously in separate Go Routine
	go func() {
		defer func() {
			// Initialize processing flag
			ProcessingMutex.Lock()
			delete(ProcessingNodes, nodeObj.Name)
			ProcessingMutex.Unlock()
		}()

		// 30 second timeout for background processing
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Process CPU pressure only when isCpuBusy=true
		// CPU recovery when isCpuBusy=false is handled directly by controller
		if isCpuBusy {
			e.HandleNodeCPUPressure(bgCtx, nodeObj.Name)
		}
	}()

	return []reconcile.Request{}
}

// EvictPod evicts a pod with proper labeling
func (e *EventHandler) EvictPod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.Log.WithValues("McKube/rt.evictPod", "Eviction")

	// Step 1: Add label "evicted" to the pod before eviction
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels["evicted"] = "true"

	// Update the pod with the new label
	if err := e.Update(ctx, pod); err != nil {
		logger.Error(err, "Failed to add evicted label to pod",
			"pod", pod.Name,
			"namespace", pod.Namespace)
		// Continue with eviction even if labeling fails
	}

	// Step 2: Perform the eviction
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	return e.SubResource("eviction").Create(ctx, pod, ev)
}

// ===================== Helper functions =====================

// CollectSortedTiers collects and sorts criticality tiers from pods
func CollectSortedTiers(pods []*corev1.Pod, rtData map[string]RealTimeData) []string {
	seen := map[string]bool{}
	for _, p := range pods {
		app := p.Labels["sdv.com"]
		if app == "" {
			continue
		}
		if rt, ok := rtData[app]; ok {
			if _, ok2 := CriticalityRank[rt.Criticality]; ok2 {
				seen[rt.Criticality] = true
			}
		}
	}
	var tiers []string
	for c := range seen {
		tiers = append(tiers, c)
	}
	// ascending by rank
	for i := 0; i < len(tiers); i++ {
		for j := i + 1; j < len(tiers); j++ {
			if CriticalityRank[tiers[j]] < CriticalityRank[tiers[i]] {
				tiers[i], tiers[j] = tiers[j], tiers[i]
			}
		}
	}
	return tiers
}

// ReindexCurrentTier: Resynchronizes CurrentTier/Idx to match the reordered list after state.Tiers dynamically changes
func ReindexCurrentTier(state *NodePressureState) {
	if len(state.Tiers) == 0 {
		state.CurrentTier = ""
		state.CurrentTierIdx = 0
		return
	}
	// Synchronize to that position if current tier exists in list
	for i, t := range state.Tiers {
		if t == state.CurrentTier {
			state.CurrentTierIdx = i
			return
		}
	}
	// Reset to lowest tier (lowest rank) if not found
	state.CurrentTierIdx = 0
	state.CurrentTier = state.Tiers[0]
}

// NextHigherTier: Returns the tier with the lowest rank among tiers higher than the current tier (greater rank)
func NextHigherTier(cur string, tiers []string) (string, bool) {
	curRank, ok := CriticalityRank[cur]
	if !ok {
		return "", false
	}
	bestRank := 1 << 30
	best := ""
	for _, t := range tiers {
		if r, ok := CriticalityRank[t]; ok && r > curRank && r < bestRank {
			bestRank = r
			best = t
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}
