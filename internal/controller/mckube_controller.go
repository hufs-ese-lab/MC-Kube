package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/client-go/dynamic"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcoperatorv1 "mc-kube/api/v1"
	"mc-kube/internal/ipvs"
	"mc-kube/internal/cpupool"
)

// Type aliases for ipvs package types
type RealTimeData = ipvs.RealTimeData
type RealTimeWCET = ipvs.RealTimeWCET

type McKubeReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	DynamicClient  dynamic.Interface
	DataCollector  *ipvs.DataCollector
	PodSpecHandler *ipvs.PodSpecHandler
	EventHandler   *ipvs.EventHandler
}

// Track pod runtime state (low or hi)
var podRuntimeState = make(map[string]string) // podName -> "low" or "hi"
var runtimeStateMutex sync.RWMutex

// CgroupRequest for RT daemon communication
type CgroupRequest struct {
	ContainerID    string  `json:"container_id"`
	Period         int     `json:"period"`
	Runtime        int     `json:"runtime"`
	Core           *string `json:"core,omitempty"`
	OnlyRuntime    bool    `json:"only_runtime,omitempty"` // true = escalation mode (period unchanged)
}

// RTRequestSender interface for sending RT requests to nodes
type RTRequestSender interface {
	SendRTRequest(nodeIP string, req CgroupRequest) error
}

// CgroupRequest is used by Webhook, so removed from controller

// Timers maps node names to the number of ticks remaining until taint removal
var Timers = make(map[string]int)

// Taint monitoring thread polling rate (seconds)
const polling_rate = 10

// Criticality order: A < B < C
// Criticality order: Low < Middle < High
var criticalityRank = map[string]int{
	"Low":    0,
	"Middle": 1,
	"High":   2,
}

const targetNamespace = "default"

// Taint key (kept for compatibility, not required for eviction fast-path)
const rtPressureTaintKey = "McKubeRTDeadlinePressure"

// Finalizer name for McKube resources
const mckubeFinalizer = "mckube.mcoperator.sdv.com/finalizer"

// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckuberealtimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=update;patch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckuberealtime,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=pods/eviction,verbs=create

type NodePressureState struct {
	AboveSec       int
	Tiers          []string
	CurrentTierIdx int
	CurrentTier    string
	PerTier        map[string]*perTierState
}

type perTierState struct {
	ElapsedSec      int
	DegradeCount    int // Number of degradation attempts
	EvictTried      bool
	MissingTicks    int
	LastDegradeTime int // Timestamp of last degradation attempt
}

// ===================== Data structures for overrun event logging =====================
type OverrunData struct {
	NodeName    string `json:"node_name,omitempty"` // optional
	ContainerID string `json:"container_id"`        // required
	Timestamp   int64  `json:"timestamp,omitempty"` // optional
}

// ===================== Data structures for CPU Pool management =====================
// CPUPool types are defined in internal/cpupool and shared with the Webhooks.
// Type aliases keep existing Controller code compiling without name changes.
type CPUCoreInfo = cpupool.CPUCoreInfo
type PodInfo = cpupool.PodInfo
type CPUPool = cpupool.CPUPool


// Track last CPU state per node (nodeName -> isCpuBusy)
var lastCpuBusyState = make(map[string]bool)
var lastCpuBusyStateMutex sync.RWMutex

const coreUtilizationThreshold = 0.9 // 90% threshold

// ===================== Reconcile =====================

func (r *McKubeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	defer duration(track("Reconcile"))
	logger := log.Log.WithValues("McKube/rt", req.NamespacedName)
	// Lower V() numbers indicate higher priority
	// V: Verbosity level
	loggerLowPrio := logger.V(1)
	loggerHighPrio := logger.V(0)
	loggerLowPrio.Info("Mc-Kube/rt Reconcile method started")

	rt := &mcoperatorv1.McKube{}

	loggerLowPrio.Info("Fetching McKube resource")
	if err := r.Get(ctx, req.NamespacedName, rt); err != nil {
		if client.IgnoreNotFound(err) == nil {
			loggerLowPrio.Info("McKube/rt resource not found. Likely deleted.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get McKube/rt instance")
		return ctrl.Result{}, err
	}
	loggerLowPrio.Info("McKube resource fetched successfully")

	// ==================== Finalizer Processing ====================
	// Handle McKube resource deletion (DeletionTimestamp is set)
	if !rt.ObjectMeta.DeletionTimestamp.IsZero() {
		// Only perform cleanup if finalizer exists
		if containsString(rt.GetFinalizers(), mckubeFinalizer) {
			loggerHighPrio.Info("McKube resource is being deleted, performing cleanup",
				"mckube", rt.Name,
				"podName", rt.Spec.PodName)

			// Perform cleanup
			if rt.Spec.PodName != "" {
				r.cleanupPodState(rt.Spec.PodName, rt.Namespace)
			}

			// Remove finalizer
			rt.SetFinalizers(removeString(rt.GetFinalizers(), mckubeFinalizer))
			if err := r.Update(ctx, rt); err != nil {
				logger.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			loggerHighPrio.Info("Finalizer removed, McKube resource can be deleted")
		}
		return ctrl.Result{}, nil
	}

	// McKube resource is active - Add finalizer
	if !containsString(rt.GetFinalizers(), mckubeFinalizer) {
		loggerLowPrio.Info("Adding finalizer to McKube resource")
		rt.SetFinalizers(append(rt.GetFinalizers(), mckubeFinalizer))
		if err := r.Update(ctx, rt); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		loggerLowPrio.Info("Finalizer added successfully")
		return ctrl.Result{Requeue: true}, nil
	}
	// ==================== End of Finalizer Processing ====================

	if rt.Spec.PodName == "" {
		loggerHighPrio.Info("McKube resource has empty spec.PodName. Ignoring...")
		return ctrl.Result{}, nil
	}

	if rt.Spec.Node == "" {
		loggerLowPrio.Info("spec.Node is empty. Attempting to find the Pod and update Node.")

		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.PodName}, pod); err != nil {
			if client.IgnoreNotFound(err) == nil {
				loggerLowPrio.Info("Target pod not found. Requeuing...")
				return ctrl.Result{RequeueAfter: time.Second * 5}, nil
			}
			logger.Error(err, "Failed to get target pod")
			return ctrl.Result{}, err
		}

		if pod.Spec.NodeName == "" || (pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending) {
			loggerLowPrio.Info("Target pod not scheduled or not in Running/Pending phase. Requeuing...", "podPhase", pod.Status.Phase)
			return ctrl.Result{RequeueAfter: time.Second * 5}, nil
		}

		rt.Spec.Node = pod.Spec.NodeName
		if err := r.Update(ctx, rt); err != nil {
			logger.Error(err, "Failed to update McKube resource with node name")
			return ctrl.Result{}, err
		}
		loggerHighPrio.Info("McKube resource updated with Node name. Requeuing to process...")
		return ctrl.Result{RequeueAfter: time.Second * 1}, nil
	}

	// If Pod is scheduled and has RT settings, check for preemption and update CPU Pool
	if rt.Spec.RTSettings != nil {
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.PodName}, pod); err == nil {
			// Process if Pod is assigned to a node and in Pending or Running state
			if pod.Spec.NodeName != "" && (pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning) {

				// Initialize runtime state when Pod processing starts (for new Pods)
				r.ensurePodRuntimeStateInitialized(pod.Name)

				if rt.Spec.RTSettings.Core != nil {
					// Determine actual core to use: prioritize Status.AllocatedCore if present (changed due to preemption)
					var actualCoreStr string
					if rt.Status.AllocatedCore != "" {
						actualCoreStr = rt.Status.AllocatedCore
					} else {
						actualCoreStr = *rt.Spec.RTSettings.Core
						// Save to Status on initial allocation
						rt.Status.AllocatedCore = actualCoreStr
						if err := r.Status().Update(ctx, rt); err != nil {
							logger.Error(err, "Failed to initialize allocated core in status")
						}
					}
					
					targetCores := cpupool.ParseCoreSet(actualCoreStr)

					// Calculate CPU usage based on RT settings
					runtimeStateMutex.RLock()
					currentRuntimeState := podRuntimeState[pod.Name]
					runtimeStateMutex.RUnlock()

					var effectiveRuntime int
					if currentRuntimeState == "hi" {
						effectiveRuntime = rt.Spec.RTSettings.RuntimeHi
					} else {
						effectiveRuntime = rt.Spec.RTSettings.RuntimeLow
					}

					cpuMillis := int64(0)
					if rt.Spec.RTSettings.Period > 0 {
						cpuMillis = int64(float64(effectiveRuntime) / float64(rt.Spec.RTSettings.Period) * 1000.0)
					}
					if cpuMillis == 0 {
						cpuMillis = 100 // Default value
					}

					criticality := rt.Spec.Criticality

					// Check if already registered in CPU Pool with the same configuration
					pool := cpupool.GetOrCreateCPUPool(pod.Spec.NodeName, cpupool.DefaultNumCores)
					alreadyRegistered := true
					pool.Mu.RLock()
					for _, coreID := range targetCores {
						if core, exists := pool.Cores[coreID]; exists {
							if existingPod, podExists := core.Pods[pod.Name]; !podExists ||
								existingPod.CPUMillis != cpuMillis ||
								existingPod.Criticality != criticality {
								alreadyRegistered = false
								break
							}
						} else {
							alreadyRegistered = false
							break
						}
					}
					pool.Mu.RUnlock()

					// Perform preemption check and update only if not already registered
					if !alreadyRegistered {
						// Check for preemption first (before adding to CPU Pool)
						loggerHighPrio.Info("Checking preemption for pod",
							"pod", pod.Name,
							"cores", targetCores,
							"cpuMillis", cpuMillis,
							"criticality", criticality)

						if err := r.checkAndPreemptForPod(ctx, pod, targetCores, cpuMillis, criticality); err != nil {
							logger.Error(err, "Failed to check preemption for pod", "pod", pod.Name)
						}

						// Update CPU Pool after preemption check
						if err := r.updateCPUPoolForPod(ctx, pod, rt); err != nil {
							logger.Error(err, "Failed to update CPU pool for pod", "pod", pod.Name)
						}
					}
				} else {
					// Update CPU Pool only without preemption check
					if err := r.updateCPUPoolForPod(ctx, pod, rt); err != nil {
						logger.Error(err, "Failed to update CPU pool for pod", "pod", pod.Name)
					}
				}

				// Apply RT settings to containers
				if pod.Status.Phase == corev1.PodRunning {
					if err := r.applyRTSettingsToContainers(ctx, pod, rt); err != nil {
						logger.Error(err, "Failed to apply RT settings to containers", "pod", pod.Name)
					}
				}
			}
		}
	}

	// Check and handle node CPU pressure
	if rt.Spec.Node != "" {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: rt.Spec.Node}, node); err == nil {
			ann := node.GetAnnotations()
			if ann != nil {
				// Check CPU pressure status
				if isCpuBusyStr, exists := ann[ipvs.AnnCpuBusyKey]; exists {
					isCpuBusy := strings.TrimSpace(isCpuBusyStr) == "true"

					// Process only when state has changed compared to previous state
					lastCpuBusyStateMutex.RLock()
					lastState, hasLastState := lastCpuBusyState[rt.Spec.Node]
					lastCpuBusyStateMutex.RUnlock()

					// Process only when state has changed or checking for the first time
					if !hasLastState || lastState != isCpuBusy {
						lastCpuBusyStateMutex.Lock()
						lastCpuBusyState[rt.Spec.Node] = isCpuBusy
						lastCpuBusyStateMutex.Unlock()

						if isCpuBusy {
							
							loggerHighPrio.Info("CPU pressure detected, handling with EventHandler", "node", rt.Spec.Node)
							r.EventHandler.HandleNodeCPUPressure(ctx, rt.Spec.Node)
						} else {
							
							loggerHighPrio.Info("CPU recovered, handling with controller", "node", rt.Spec.Node)
							r.handleCPURecovery(ctx, rt.Spec.Node)
						}
					}
				}
			}
		}
	}

	loggerLowPrio.Info("Reconcile method finished")
	return ctrl.Result{}, nil
}

// All existing IPVS-related logic has been migrated to the ipvs package

// handleCPURecovery: Reverts all RT pods to runtime_low when CPU normalizes (isCpuBusy=false)
func (r *McKubeReconciler) handleCPURecovery(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPURecovery", "Recovery", "node", nodeName)
	logger.V(0).Info("CPU recovered (isCpuBusy=false), reverting pods to runtime_low")

	// Query all Pods on the node
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
		logger.Error(err, "Failed to list pods", "namespace", targetNamespace)
		return
	}

	var nodePods []*corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Spec.NodeName == nodeName {
			// Process only RT Pods with sdv.com label
			if p.Labels != nil && p.Labels["sdv.com"] != "" {
				nodePods = append(nodePods, p)
			}
		}
	}

	if len(nodePods) == 0 {
		logger.V(1).Info("No RT pods found on node")
		return
	}

	logger.V(0).Info("Found RT pods to revert", "count", len(nodePods))

	// Revert each Pod to runtime_low
	for _, pod := range nodePods {
		// Check current runtime state
		runtimeStateMutex.RLock()
		currentState := podRuntimeState[pod.Name]
		runtimeStateMutex.RUnlock()

		if currentState != "hi" {
			// Already in low state or not configured
			logger.V(1).Info("Pod not in hi state, skipping", "pod", pod.Name)
			continue
		}

		// Query McKube CR
		mckubeList := &mcoperatorv1.McKubeList{}
		if err := r.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
			logger.Error(err, "Failed to list McKube resources", "pod", pod.Name)
			continue
		}

		var targetMcKube *mcoperatorv1.McKube
		for i := range mckubeList.Items {
			if mckubeList.Items[i].Spec.PodName == pod.Name {
				targetMcKube = &mckubeList.Items[i]
				break
			}
		}

		if targetMcKube == nil || targetMcKube.Spec.RTSettings == nil {
			logger.V(1).Info("No McKube CR or RT settings found for pod", "pod", pod.Name)
			continue
		}

		
		logger.V(0).Info("Reverting pod runtime from hi to low",
			"pod", pod.Name,
			"runtime_hi", targetMcKube.Spec.RTSettings.RuntimeHi,
			"runtime_low", targetMcKube.Spec.RTSettings.RuntimeLow)

		// Apply runtime_low to all containers
		nodeIP := pod.Status.HostIP
		if nodeIP == "" {
			logger.Error(fmt.Errorf("node IP not available"), "Failed to get node IP", "pod", pod.Name)
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.ContainerID == "" {
				continue
			}

			req := CgroupRequest{
				ContainerID: cs.ContainerID,
				Period:      targetMcKube.Spec.RTSettings.Period,  // Passed for reference only
				Runtime:     targetMcKube.Spec.RTSettings.RuntimeLow,
				Core:        targetMcKube.Spec.RTSettings.Core,
				OnlyRuntime: true,  // Change only runtime even during CPU recovery
			}

			if err := r.SendRTRequest(nodeIP, req); err != nil {
				logger.Error(err, "Failed to apply runtime_low to container",
					"containerID", cs.ContainerID,
					"pod", pod.Name)
				continue
			}

			logger.V(0).Info("Successfully reverted container to runtime_low",
				"pod", pod.Name,
				"container", cs.Name,
				"runtime", targetMcKube.Spec.RTSettings.RuntimeLow)
		}

		// Update state
		runtimeStateMutex.Lock()
		podRuntimeState[pod.Name] = "low"
		runtimeStateMutex.Unlock()

		// Update McKube CR status
		targetMcKube.Status.CurrentRuntime = "low"
		if err := r.Status().Update(ctx, targetMcKube); err != nil {
			logger.Error(err, "Failed to update McKube status", "pod", pod.Name)
		}
	}

	// Clear node pressure state after CPU recovery is complete
	ipvs.ProcessingMutex.Lock()
	if _, exists := ipvs.PressureState[nodeName]; exists {
		delete(ipvs.PressureState, nodeName)
		logger.V(0).Info("Cleared node pressure state after CPU recovery")
	}
	if _, exists := ipvs.ProcessingNodes[nodeName]; exists {
		delete(ipvs.ProcessingNodes, nodeName)
		logger.V(0).Info("Cleared node processing state after CPU recovery")
	}
	ipvs.ProcessingMutex.Unlock()

	logger.V(0).Info("CPU recovery completed", "podsReverted", len(nodePods))
}

// ===================== Utils / timing =====================

// track() & duration() : Function to measure function execution time
func track(msg string) (string, time.Time) {
	return msg, time.Now()
}

var max time.Duration = 0
var counter int = 1

func duration(msg string, start time.Time) {
	elapsed := time.Since(start)
	if counter > 1 {
		if elapsed > max {
			max = elapsed
		}
	}
	if counter%50 == 0 {
		log.Log.V(0).Info("Time", msg, elapsed, "Max", max)
		counter = 1
	}
	counter++
}

// ===================== Setup =====================

// StartTaintThread: Thread function for monitoring and releasing node taints
func (r *McKubeReconciler) StartTaintThread() {
	go func() {
		logger := log.Log.WithValues("McKube/rt.TaintMonitoringThread", "Taint")
		logger.V(1).Info("Starting taint monitoring thread")
		for {
			time.Sleep(time.Duration(polling_rate) * time.Second)
			logger.V(1).Info("Taint Thread: Waking up, working...", "len(Timers)", len(Timers))
			for nodeName, timer := range Timers {
				if timer <= 0 {
					node := &corev1.Node{}
					err := r.Get(context.TODO(), types.NamespacedName{Name: nodeName}, node)
					if err != nil {
						if k8serrors.IsNotFound(err) {
							logger.Error(err, "Taint Thread: node not found, ignoring...")
							continue
						}
						logger.Error(err, "Taint Thread: failed to get node instance")
						continue
					}
					for i, taint := range node.Spec.Taints {
						if taint.Key == rtPressureTaintKey {
							node.Spec.Taints[i] = node.Spec.Taints[len(node.Spec.Taints)-1]
							node.Spec.Taints = node.Spec.Taints[:len(node.Spec.Taints)-1]
							log.Log.V(0).Info("Taint Thread: untainting node", "node", nodeName)
							err = r.Update(context.TODO(), node)
							if err != nil {
								logger.Error(err, "Taint Thread: error while un-tainting the node")
							}
							break
						}
					}
					delete(Timers, nodeName)
				} else {
					logger.V(0).Info("Decrementing timer", nodeName, Timers[nodeName])
					Timers[nodeName]--
				}
			}
		}
	}()
}

// SetupWithManager: Registers the Reconciler with the manager
func (r *McKubeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize DataCollector
	r.DataCollector = ipvs.NewDataCollector(r.Client, r.DynamicClient)

	// Initialize PodSpecHandler
	r.PodSpecHandler = ipvs.NewPodSpecHandler(r.Client)

	// Initialize EventHandler
	r.EventHandler = ipvs.NewEventHandler(r.Client, r.DataCollector, r.PodSpecHandler)

	// Index Pods by their name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, ".metadata.name", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Name}
	}); err != nil {
		return err
	}

	r.StartOverrunListener(8090) // Declare Overrun listening port

	return ctrl.NewControllerManagedBy(mgr).
		For(&mcoperatorv1.McKube{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(handler.MapFunc(r.findObjectsForPod)),
		).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(handler.MapFunc(r.EventHandler.FindObjectsForNode)),
		).
		Complete(r)
}

// findObjectsForPod() : Finds McKube-related CRs in the namespace where the pod was created and generates Reconcile requests
func (r *McKubeReconciler) findObjectsForPod(ctx context.Context, pod client.Object) []reconcile.Request {
	if pod.GetNamespace() != targetNamespace {
		return []reconcile.Request{}
	}

	// Check sdv.com label - Do not process if not RT workload
	labels := pod.GetLabels()
	if labels == nil || labels["sdv.com"] == "" {
		return []reconcile.Request{}
	}

	// Clean up internal state when Pod is deleted
	if pod.GetDeletionTimestamp() != nil {
		r.cleanupPodState(pod.GetName(), pod.GetNamespace())
	}

	mckubeList := &mcoperatorv1.McKubeList{}
	if err := r.List(ctx, mckubeList, client.InNamespace(pod.GetNamespace())); err != nil {
		log.Log.Error(err, "Failed to list McKube resources in findObjectsForPod")
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	for _, mckube := range mckubeList.Items {
		if mckube.Spec.PodName == pod.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      mckube.Name,
					Namespace: mckube.Namespace,
				},
			})
			return requests
		}
	}
	return []reconcile.Request{}
}

// cleanupPodState: Cleans up all internal state when a Pod is deleted
func (r *McKubeReconciler) cleanupPodState(podName, namespace string) {
	logger := log.Log.WithValues("McKube/rt.Cleanup", "PodState", "pod", podName)
	logger.V(0).Info("Cleaning up internal state for deleted pod")

	// 1. Clean up Controller's podRuntimeState
	runtimeStateMutex.Lock()
	if _, exists := podRuntimeState[podName]; exists {
		delete(podRuntimeState, podName)
		logger.V(0).Info("Removed pod from runtime state tracking")
	}
	runtimeStateMutex.Unlock()

	// 2. Remove the Pod from the shared in-memory CPU Pool.
	cpupool.RemovePodFromPool(podName)
	logger.V(0).Info("Removed pod from shared CPU pool")

	logger.V(0).Info("Pod state cleanup completed")
}

// cleanupPodStateByMcKubeName: Cleans up pod state by McKube CR name
func (r *McKubeReconciler) cleanupPodStateByMcKubeName(ctx context.Context, mcKubeName types.NamespacedName) {
	logger := log.Log.WithValues("McKube/rt.Cleanup", "McKubeDeleted", "mckube", mcKubeName.Name)

	// Since McKube CR is deleted, it is difficult to infer podName from the name
	// Instead of cleaning up all states, check the Pods in that namespace
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(mcKubeName.Namespace)); err != nil {
		logger.Error(err, "Failed to list pods for cleanup")
		return
	}

	// Clean up state of non-existent Pods
	existingPods := make(map[string]bool)
	for _, pod := range podList.Items {
		if pod.Labels != nil && pod.Labels["sdv.com"] != "" {
			existingPods[pod.Name] = true
		}
	}

	// Clean up Controller podRuntimeState
	runtimeStateMutex.Lock()
	for podName := range podRuntimeState {
		if !existingPods[podName] {
			delete(podRuntimeState, podName)
			logger.V(0).Info("Cleaned up runtime state for non-existent pod", "pod", podName)
		}
	}
	runtimeStateMutex.Unlock()

	// Clean up shared in-memory CPU Pool.
	cpupool.RemovePodsNotIn(existingPods)
	logger.V(0).Info("Cleaned up shared CPU pool for non-existent pods")

	logger.V(0).Info("McKube deletion cleanup completed")
}

// ensurePodRuntimeStateInitialized: Checks and initializes pod runtime state
func (r *McKubeReconciler) ensurePodRuntimeStateInitialized(podName string) {
	runtimeStateMutex.Lock()
	if _, exists := podRuntimeState[podName]; !exists {
		podRuntimeState[podName] = "low" // Set to low as default value
		log.Log.V(0).Info("Initialized pod runtime state to low", "pod", podName)
	}
	runtimeStateMutex.Unlock()
}

// ===================== RT configuration functions are handled by Webhook =====================
// Duplicate processing prevention: RT configuration-related functions removed from controller

// ===================== Overrun Listening Thread =====================

// StartOverrunListener: Starts HTTP server for receiving overrun events
func (r *McKubeReconciler) StartOverrunListener(port int) {
	go func() {
		logger := log.Log.WithValues("McKube/rt.OverrunListener", "HTTP")
		logger.V(0).Info("Starting overrun listener", "port", port)

		http.HandleFunc("/overrun", func(w http.ResponseWriter, req *http.Request) {
			if req.Method != http.MethodPost {
				logger.V(1).Info("Invalid method for /overrun", "method", req.Method)
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			var data OverrunData
			decoder := json.NewDecoder(req.Body)
			if err := decoder.Decode(&data); err != nil {
				logger.Error(err, "Failed to decode overrun data")
				http.Error(w, "Invalid JSON", http.StatusBadRequest)
				return
			}

			logger.V(0).Info("Received overrun event",
				"node", data.NodeName,
				"containerID", data.ContainerID,
				"timestamp", data.Timestamp)

			r.handleOverrunEvent(data)

			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})

		addr := fmt.Sprintf(":%d", port)
		logger.V(0).Info("Overrun listener ready", "address", addr)

		if err := http.ListenAndServe(addr, nil); err != nil {
			logger.Error(err, "Overrun listener failed to start")
		}
	}()
}

// handleOverrunEvent: Handles overrun event processing
func (r *McKubeReconciler) handleOverrunEvent(data OverrunData) {
	logger := log.Log.WithValues("McKube/rt.OverrunHandler", "Overrun")
	ctx := context.TODO()

	logger.V(0).Info("=== Overrun Event Detected ===",
		"nodeName", data.NodeName,
		"containerID", data.ContainerID,
		"timestamp", data.Timestamp)

	pod, err := r.findPodByContainerID(ctx, data.NodeName, data.ContainerID)
	if err != nil {
		logger.Error(err, "Failed to find pod for container",
			"containerID", data.ContainerID,
			"nodeName", data.NodeName)
		return
	}

	if pod == nil {
		logger.V(0).Info("No pod found for container ID",
			"containerID", data.ContainerID,
			"nodeName", data.NodeName)
		return
	}

	containerName := ""
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == data.ContainerID {
			containerName = cs.Name
			break
		}
	}

	
	logger.V(0).Info("=== Overrun Pod Identified ===",
		"podName", pod.Name,
		"namespace", pod.Namespace,
		"nodeName", pod.Spec.NodeName,
		"containerName", containerName,
		"containerID", data.ContainerID,
		"podPhase", pod.Status.Phase,
		"timestamp", data.Timestamp)

	mckubeList := &mcoperatorv1.McKubeList{}
	if err := r.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		logger.Error(err, "Failed to list McKube resources")
		return
	}

	var targetMcKube *mcoperatorv1.McKube
	for i := range mckubeList.Items {
		if mckubeList.Items[i].Spec.PodName == pod.Name {
			targetMcKube = &mckubeList.Items[i]
			break
		}
	}

	if targetMcKube == nil {
		logger.V(0).Info("No McKube CR found for pod", "podName", pod.Name)
		return
	}

	if targetMcKube.Spec.RTSettings == nil {
		logger.V(0).Info("Pod has no RT settings configured", "podName", pod.Name)
		return
	}

	if app, ok := pod.Labels["sdv.com"]; ok {
		rtData, err := r.DataCollector.GetRealTimeData(ctx)
		if err == nil {
			if rt, found := rtData[app]; found {
				logger.V(0).Info("Pod RT Information",
					"podName", pod.Name,
					"criticality", rt.Criticality,
					"rtPeriod", rt.RTPeriod,
					"rtDeadline", rt.RTDeadline)
			}
		}
	}

	runtimeStateMutex.RLock()
	currentState := podRuntimeState[pod.Name]
	runtimeStateMutex.RUnlock()

	if currentState == "hi" {
		logger.V(0).Info("Pod already using runtime_hi, no action needed",
			"podName", pod.Name)
		return
	}

	logger.V(0).Info("Escalating pod runtime from low to hi due to overrun",
		"podName", pod.Name,
		"runtime_low", targetMcKube.Spec.RTSettings.RuntimeLow,
		"runtime_hi", targetMcKube.Spec.RTSettings.RuntimeHi)

	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		logger.Error(fmt.Errorf("node IP not available"), "Failed to get node IP for pod", "podName", pod.Name)
		return
	}

	req := CgroupRequest{
		ContainerID: data.ContainerID,
		Period:      targetMcKube.Spec.RTSettings.Period,
		Runtime:     targetMcKube.Spec.RTSettings.RuntimeHi,
		Core:        targetMcKube.Spec.RTSettings.Core,
		OnlyRuntime: true,  // Escalation mode: change runtime only, period unchanged
	}

	if err := r.SendRTRequest(nodeIP, req); err != nil {
		coreStr := "nil"
		if req.Core != nil {
			coreStr = *req.Core
		}
		logger.Error(err, "Failed to apply runtime_hi to container",
			"containerID", data.ContainerID,
			"podName", pod.Name,
			"period", req.Period,
			"runtime", req.Runtime,
			"core", coreStr,
			"onlyRuntime", req.OnlyRuntime,
			"nodeIP", nodeIP)
		return
	}

	runtimeStateMutex.Lock()
	podRuntimeState[pod.Name] = "hi"
	runtimeStateMutex.Unlock()

	now := metav1.Now()
	targetMcKube.Status.CurrentRuntime = "hi"
	targetMcKube.Status.LastOverrunTime = &now
	if err := r.Status().Update(ctx, targetMcKube); err != nil {
		logger.Error(err, "Failed to update McKube status", "podName", pod.Name)
	}

	logger.V(0).Info("Successfully escalated pod runtime to hi",
		"podName", pod.Name,
		"newRuntime", targetMcKube.Spec.RTSettings.RuntimeHi)
}

// findPodByContainerID: Finds a pod by container ID on a specific node
func (r *McKubeReconciler) findPodByContainerID(ctx context.Context, nodeName string, containerID string) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	// Normalize container ID (format: containerd://abc123 or docker://abc123)
	normalizedID := containerID
	if idx := strings.Index(containerID, "://"); idx != -1 {
		normalizedID = containerID[idx+3:]
	}

	for i := range podList.Items {
		pod := &podList.Items[i]

		// Filter by node if specified
		if nodeName != "" && pod.Spec.NodeName != nodeName {
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			statusID := cs.ContainerID
			if idx := strings.Index(statusID, "://"); idx != -1 {
				statusID = statusID[idx+3:]
			}

			if cs.ContainerID == containerID ||
				statusID == normalizedID ||
				strings.Contains(cs.ContainerID, normalizedID) {
				return pod, nil
			}
		}

		for _, cs := range pod.Status.InitContainerStatuses {
			statusID := cs.ContainerID
			if idx := strings.Index(statusID, "://"); idx != -1 {
				statusID = statusID[idx+3:]
			}

			if cs.ContainerID == containerID ||
				statusID == normalizedID ||
				strings.Contains(cs.ContainerID, normalizedID) {
				return pod, nil
			}
		}
	}

	return nil, nil
}

// SendRTRequest sends RT configuration request to daemon (implements RTRequestSender interface)
func (r *McKubeReconciler) SendRTRequest(nodeIP string, req CgroupRequest) error {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %v", err)
	}

	url := fmt.Sprintf("http://%s:8080/cgroup", nodeIP)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return fmt.Errorf("failed to send request to %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("node-actuator request failed with status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// ===================== Preemption Logic =====================

// checkAndPreemptForPod: Check and execute preemption before Pod allocation
// High criticality: Can preempt Middle/Low
// Middle criticality: Can preempt Low
func (r *McKubeReconciler) checkAndPreemptForPod(ctx context.Context, pod *corev1.Pod, targetCores []int, cpuMillis int64, criticality string) error {
	logger := log.Log.WithValues("McKube/rt.Preemption", "Check")

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod has no assigned node")
	}

	pool := cpupool.GetPoolForNode(nodeName)
	exists := pool != nil

	if !exists {
		logger.V(1).Info("No CPU pool for node, skipping preemption check", "node", nodeName)
		return nil
	}

	
	for _, coreID := range targetCores {
		
		currentUsage := pool.GetCoreUtilization(coreID)
		afterUsage := currentUsage + float64(cpuMillis)/1000.0

		logger.V(0).Info("Core utilization check",
			"core", coreID,
			"currentUsage", fmt.Sprintf("%.2f%%", currentUsage*100),
			"afterUsage", fmt.Sprintf("%.2f%%", afterUsage*100),
			"threshold", fmt.Sprintf("%.2f%%", coreUtilizationThreshold*100))

		
		if afterUsage > coreUtilizationThreshold {
			// Calculate the exact budget that must be freed to get below the threshold.
			needed := afterUsage - coreUtilizationThreshold

			logger.V(0).Info("Core utilization will exceed threshold, attempting preemption",
				"core", coreID,
				"pod", pod.Name,
				"criticality", criticality,
				"neededBudget", fmt.Sprintf("%.3f", needed))

			// Collect all lower-criticality pods on this core as candidates.
			candidates := r.findPreemptionVictims(pool, coreID, criticality)
			if len(candidates) == 0 {
				logger.V(0).Info("No preemptable victims found on core",
					"core", coreID,
					"criticality", criticality)
				continue
			}

			// Select only the minimum set of victims required to cover needed budget.
			// Order: lowest criticality first, then largest budget first (greedy).
			victims := cpupool.SelectMinimumVictims(candidates, needed)

			logger.V(0).Info("Selected minimum preemption victims",
				"core", coreID,
				"candidateCount", len(candidates),
				"selectedCount", len(victims))

			for _, victim := range victims {
				if err := r.preemptPod(ctx, victim, pool, coreID); err != nil {
					logger.Error(err, "Failed to preempt victim pod",
						"victim", victim.Name,
						"core", coreID)
				} else {
					logger.V(0).Info("Successfully preempted victim pod",
						"victim", victim.Name,
						"victimCriticality", victim.Criticality,
						"preemptor", pod.Name,
						"preemptorCriticality", criticality,
						"core", coreID)
				}
			}
		}
	}

	return nil
}

// findPreemptionVictims: Find preemptable Pods
// High can preempt Middle, Low
// Middle can preempt Low
func (r *McKubeReconciler) findPreemptionVictims(pool *CPUPool, coreID int, preemptorCriticality string) []PodInfo {
	pods := pool.GetPodsOnCore(coreID)
	victims := make([]PodInfo, 0)

	preemptorRank := criticalityRank[preemptorCriticality]

	for _, pod := range pods {
		victimRank := criticalityRank[pod.Criticality]

		// Preemption allowed if preemptor has higher priority (larger rank) than victim
		if preemptorRank > victimRank {
			victims = append(victims, pod)
		}
	}

	return victims
}

// preemptPod: Preempt Pod by migrating to another core or evicting
func (r *McKubeReconciler) preemptPod(ctx context.Context, victim PodInfo, pool *CPUPool, currentCore int) error {
	logger := log.Log.WithValues("McKube/rt.Preemption", "Evict")

	logger.V(0).Info("Preempting pod from core",
		"pod", victim.Name,
		"namespace", victim.Namespace,
		"criticality", victim.Criticality,
		"core", currentCore)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      victim.Name,
		Namespace: victim.Namespace,
	}, pod); err != nil {
		return fmt.Errorf("failed to get victim pod: %v", err)
	}

	
	pool.RemovePodFromCore(currentCore, victim.Name)

	
	newCore := pool.FindLeastLoadedCore()
	newCoreUsage := pool.GetCoreUtilization(newCore)

	logger.V(0).Info("Attempting to migrate pod to different core",
		"pod", victim.Name,
		"fromCore", currentCore,
		"toCore", newCore,
		"newCoreUsage", fmt.Sprintf("%.2f%%", newCoreUsage*100))

	
	if newCoreUsage+float64(victim.CPUMillis)/1000.0 <= coreUtilizationThreshold {
		pool.AddPodToCore(newCore, victim)

		
		if err := r.updatePodCoreAffinity(ctx, pod, newCore); err != nil {
			logger.Error(err, "Failed to update pod core affinity",
				"pod", victim.Name,
				"newCore", newCore)
			return err
		}

		logger.V(0).Info("Successfully migrated pod to different core",
			"pod", victim.Name,
			"fromCore", currentCore,
			"toCore", newCore)

		return nil
	}

	
	logger.V(0).Info("No available core for migration, evicting pod",
		"pod", victim.Name,
		"criticality", victim.Criticality)

	return r.EventHandler.EvictPod(ctx, pod)
}


func (r *McKubeReconciler) updatePodCoreAffinity(ctx context.Context, pod *corev1.Pod, newCore int) error {
	logger := log.Log.WithValues("McKube/rt.CoreUpdate", "Affinity")

	mckubeList := &mcoperatorv1.McKubeList{}
	if err := r.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		return fmt.Errorf("failed to list McKube resources: %v", err)
	}

	var targetMcKube *mcoperatorv1.McKube
	for i := range mckubeList.Items {
		if mckubeList.Items[i].Spec.PodName == pod.Name {
			targetMcKube = &mckubeList.Items[i]
			break
		}
	}

	if targetMcKube == nil || targetMcKube.Spec.RTSettings == nil {
		logger.V(1).Info("No McKube CR or RT settings found for pod", "pod", pod.Name)
		return nil
	}

	
	newCoreStr := fmt.Sprintf("%d", newCore)
	targetMcKube.Status.AllocatedCore = newCoreStr

	if err := r.Status().Update(ctx, targetMcKube); err != nil {
		return fmt.Errorf("failed to update McKube status: %v", err)
	}

	logger.V(0).Info("Updated pod core affinity",
		"pod", pod.Name,
		"newCore", newCore)

	
	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		return fmt.Errorf("node IP not available")
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == "" {
			continue
		}

		req := CgroupRequest{
			ContainerID: cs.ContainerID,
			Period:      targetMcKube.Spec.RTSettings.Period,
			Runtime:     targetMcKube.Spec.RTSettings.RuntimeLow,
			Core:        &newCoreStr,
		}

		if err := r.SendRTRequest(nodeIP, req); err != nil {
			logger.Error(err, "Failed to apply new core to container",
				"containerID", cs.ContainerID,
				"pod", pod.Name)
			continue
		}
	}

	return nil
}

// ===================== Helper Functions =====================
// ParseCoreSet is provided by mc-kube/internal/cpupool package.


func (r *McKubeReconciler) updateCPUPoolForPod(ctx context.Context, pod *corev1.Pod, mckube *mcoperatorv1.McKube) error {
	if mckube.Spec.RTSettings == nil || mckube.Spec.RTSettings.Core == nil {
		return nil
	}

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod has no assigned node")
	}

	
	pool := cpupool.GetOrCreateCPUPool(nodeName, cpupool.DefaultNumCores)

	
	
	
	
	runtimeStateMutex.RLock()
	currentRuntimeState := podRuntimeState[pod.Name]
	runtimeStateMutex.RUnlock()

	var effectiveRuntime int
	if currentRuntimeState == "hi" {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeHi
	} else {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeLow
	}

	
	cpuMillis := int64(0)
	if mckube.Spec.RTSettings.Period > 0 {
		cpuMillis = int64(float64(effectiveRuntime) / float64(mckube.Spec.RTSettings.Period) * 1000.0)
	}

	
	if cpuMillis == 0 {
		cpuMillis = 100 
	}

	
	podInfo := PodInfo{
		Name:        pod.Name,
		Namespace:   pod.Namespace,
		Criticality: mckube.Spec.Criticality,
		CPUMillis:   cpuMillis,
		CoreSet:     cpupool.ParseCoreSet(*mckube.Spec.RTSettings.Core),
	}

	
	needsUpdate := false
	for _, coreID := range podInfo.CoreSet {
		
		pool.Mu.RLock()
		core, exists := pool.Cores[coreID]
		var existingPod PodInfo
		var podExists bool
		if exists {
			existingPod, podExists = core.Pods[pod.Name]
		}
		pool.Mu.RUnlock()

		
		if !exists || !podExists ||
			existingPod.CPUMillis != podInfo.CPUMillis ||
			existingPod.Criticality != podInfo.Criticality {
			needsUpdate = true
			break
		}
	}

	
	if needsUpdate {
		for _, coreID := range podInfo.CoreSet {
			
			pool.RemovePodFromCore(coreID, pod.Name)
			pool.AddPodToCore(coreID, podInfo)
		}

		log.Log.V(0).Info("Updated CPU pool for pod",
			"pod", pod.Name,
			"node", nodeName,
			"cores", podInfo.CoreSet,
			"cpuMillis", cpuMillis,
			"runtime", effectiveRuntime,
			"period", mckube.Spec.RTSettings.Period,
			"criticality", podInfo.Criticality)
	}

	return nil
}

// applyRTSettingsToContainers applies RT cgroup settings to all containers in a pod
func (r *McKubeReconciler) applyRTSettingsToContainers(ctx context.Context, pod *corev1.Pod, mckube *mcoperatorv1.McKube) error {
	logger := log.Log.WithValues("McKube/rt.RTSettings", "Apply")

	if mckube.Spec.RTSettings == nil {
		return nil
	}

	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		return fmt.Errorf("node IP not available")
	}

	
	runtimeStateMutex.RLock()
	currentRuntimeState := podRuntimeState[pod.Name]
	runtimeStateMutex.RUnlock()

	var effectiveRuntime int
	if currentRuntimeState == "hi" {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeHi
	} else {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeLow
	}

	
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == "" {
			continue
		}

		
		var coreToUse *string
		if mckube.Status.AllocatedCore != "" {
			coreToUse = &mckube.Status.AllocatedCore
		} else {
			coreToUse = mckube.Spec.RTSettings.Core
		}

		req := CgroupRequest{
			ContainerID: cs.ContainerID,
			Period:      mckube.Spec.RTSettings.Period,
			Runtime:     effectiveRuntime,
			Core:        coreToUse,
		}

		if err := r.SendRTRequest(nodeIP, req); err != nil {
			logger.Error(err, "Failed to apply RT settings to container",
				"containerID", cs.ContainerID,
				"pod", pod.Name,
				"runtime", effectiveRuntime,
				"period", mckube.Spec.RTSettings.Period,
				"core", *coreToUse)
			return err
		}

		logger.V(0).Info("Applied RT settings to container",
			"pod", pod.Name,
			"container", cs.Name,
			"runtime", effectiveRuntime,
			"period", mckube.Spec.RTSettings.Period,
			"core", *coreToUse)
	}

	return nil
}

// ===================== Finalizer Helper Functions =====================

// containsString checks if a slice contains a string
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// removeString removes a string from a slice
func removeString(slice []string, s string) []string {
	result := []string{}
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

