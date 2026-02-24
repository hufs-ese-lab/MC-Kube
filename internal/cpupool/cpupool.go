// Package cpupool provides shared RT CPU budget management used by both
// the Controller and the Admission Webhooks.
//
// A process-wide singleton (globalPools) holds the per-node CPUPool state.
// The Controller writes to the pool via GetOrCreateCPUPool / AddPodToCore /
// RemovePodFromCore etc.  The Webhooks read the same in-memory data directly —
// no CRD round-trip needed.
package cpupool

import (
"fmt"
"sort"
"strconv"
"strings"
"sync"

corev1 "k8s.io/api/core/v1"
)

// RTCapacityPerCore is the maximum schedulable RT budget per core.
// Linux enforces a global 95 % RT throttle by default (kernel.sched_rt_runtime_us).
const RTCapacityPerCore = 0.95

// DefaultNumCores is the fallback when a node's CPU count cannot be determined.
const DefaultNumCores = 16

// CriticalityRank maps criticality strings to comparable integers.
// Higher value = higher priority.
var CriticalityRank = map[string]int{
"Low":    0,
"Middle": 1,
"High":   2,
}

// ── Shared data types (previously in controller package) ─────────────────────

// CPUCoreInfo holds RT CPU accounting for one physical core.
type CPUCoreInfo struct {
CoreID      int
UsageMillis int64              // currently allocated CPU (millicores)
Pods        map[string]PodInfo // podName → PodInfo
}

// PodInfo describes an RT pod's allocation on a core.
type PodInfo struct {
Name        string
Namespace   string
Criticality string // "Low" | "Middle" | "High"
CPUMillis   int64  // millicore budget (runtime/period * 1000)
CoreSet     []int  // assigned core IDs
}

// CPUPool manages per-core RT budget for one node.
// Mu is exported so callers outside the package can safely lock when iterating.
type CPUPool struct {
NodeName string
Cores    map[int]*CPUCoreInfo
Mu       sync.RWMutex
}

// ── Process-wide singleton ────────────────────────────────────────────────────

var globalPools = make(map[string]*CPUPool) // nodeName → *CPUPool
var globalPoolsMu sync.RWMutex

// GetOrCreateCPUPool returns the pool for nodeName, creating it with numCores
// cores if it does not yet exist.
func GetOrCreateCPUPool(nodeName string, numCores int) *CPUPool {
globalPoolsMu.Lock()
defer globalPoolsMu.Unlock()

if pool, ok := globalPools[nodeName]; ok {
return pool
}

pool := &CPUPool{
NodeName: nodeName,
Cores:    make(map[int]*CPUCoreInfo, numCores),
}
for i := 0; i < numCores; i++ {
pool.Cores[i] = &CPUCoreInfo{
CoreID: i,
Pods:   make(map[string]PodInfo),
}
}
globalPools[nodeName] = pool
return pool
}

// GetPoolForNode returns the pool for nodeName, or nil if none exists yet.
func GetPoolForNode(nodeName string) *CPUPool {
globalPoolsMu.RLock()
defer globalPoolsMu.RUnlock()
return globalPools[nodeName]
}

// RemovePodFromPool removes podName from every core on every node.
// Called when a Pod is deleted.
func RemovePodFromPool(podName string) {
globalPoolsMu.RLock()
defer globalPoolsMu.RUnlock()

for _, pool := range globalPools {
pool.Mu.Lock()
for _, core := range pool.Cores {
if pi, ok := core.Pods[podName]; ok {
core.UsageMillis -= pi.CPUMillis
delete(core.Pods, podName)
}
}
pool.Mu.Unlock()
}
}

// RemovePodsNotIn removes from all pools any pod whose name is not in existingPods.
// Called after McKube CR deletion to purge stale entries.
func RemovePodsNotIn(existingPods map[string]bool) {
globalPoolsMu.RLock()
defer globalPoolsMu.RUnlock()

for _, pool := range globalPools {
pool.Mu.Lock()
for _, core := range pool.Cores {
for podName, pi := range core.Pods {
if !existingPods[podName] {
core.UsageMillis -= pi.CPUMillis
delete(core.Pods, podName)
}
}
}
pool.Mu.Unlock()
}
}

// ── CPUPool methods ───────────────────────────────────────────────────────────

// GetCoreUtilization returns the current utilisation of coreID as a fraction (0.0–1.0).
// 1000 millicores = 1 core = 100 %.
func (p *CPUPool) GetCoreUtilization(coreID int) float64 {
p.Mu.RLock()
defer p.Mu.RUnlock()

core, ok := p.Cores[coreID]
if !ok {
return 0.0
}
return float64(core.UsageMillis) / 1000.0
}

// AddPodToCore registers a pod's CPU allocation on coreID.
func (p *CPUPool) AddPodToCore(coreID int, pod PodInfo) {
p.Mu.Lock()
defer p.Mu.Unlock()

if core, ok := p.Cores[coreID]; ok {
core.Pods[pod.Name] = pod
core.UsageMillis += pod.CPUMillis
}
}

// RemovePodFromCore deregisters podName from coreID.
func (p *CPUPool) RemovePodFromCore(coreID int, podName string) {
p.Mu.Lock()
defer p.Mu.Unlock()

if core, ok := p.Cores[coreID]; ok {
if pi, found := core.Pods[podName]; found {
core.UsageMillis -= pi.CPUMillis
delete(core.Pods, podName)
}
}
}

// GetPodsOnCore returns all PodInfo entries assigned to coreID.
func (p *CPUPool) GetPodsOnCore(coreID int) []PodInfo {
p.Mu.RLock()
defer p.Mu.RUnlock()

core, ok := p.Cores[coreID]
if !ok {
return nil
}
pods := make([]PodInfo, 0, len(core.Pods))
for _, pi := range core.Pods {
pods = append(pods, pi)
}
return pods
}

// FindLeastLoadedCore returns the core ID with the lowest UsageMillis.
func (p *CPUPool) FindLeastLoadedCore() int {
p.Mu.RLock()
defer p.Mu.RUnlock()

minCore := -1
minUsage := int64(1<<63 - 1)
for coreID, core := range p.Cores {
if core.UsageMillis < minUsage {
minUsage = core.UsageMillis
minCore = coreID
}
}
return minCore
}

// ── Scheduling API (reads directly from the in-memory singleton) ──────────────

// FeasibilityResult is returned by SelectBestNodeAndCore.
type FeasibilityResult struct {
Feasible         bool
SelectedNodeName string
SelectedCoreID   int
// VictimPodNames lists pods that must be evicted to make room.
// Empty when the core has enough free budget without eviction.
VictimPodNames []string
}

// SelectBestNodeAndCore selects the best (node, core) for an incoming RT pod.
//
//   - nodeList contains the live Kubernetes nodes (used for core-count metadata).
//   - requiredBudget = runtime_hi / period of the incoming pod.
//   - incomingCriticality = "Low" | "Middle" | "High".
//   - preferredCores: when non-nil, only these core IDs are considered per node.
//
// The function reads globalPools directly.  Nodes without a pool yet are treated
// as fully free (first pod on that node always succeeds in direct placement).
func SelectBestNodeAndCore(
nodeList []corev1.Node,
requiredBudget float64,
incomingCriticality string,
preferredCores []int,
) FeasibilityResult {
type candidate struct {
nodeName string
coreID   int
free     float64
victims  []string
}

var direct []candidate
var withEvict []candidate

globalPoolsMu.RLock()
defer globalPoolsMu.RUnlock()

for i := range nodeList {
node := &nodeList[i]
nodeName := node.Name

numCores := DefaultNumCores
if cpuQty, ok := node.Status.Capacity[corev1.ResourceCPU]; ok {
if n := int(cpuQty.Value()); n > 0 {
numCores = n
}
}

pool := globalPools[nodeName] // may be nil

// Build list of core IDs to evaluate.
var toCheck []int
if len(preferredCores) > 0 {
toCheck = filterValidCores(preferredCores, numCores)
} else if pool != nil {
pool.Mu.RLock()
toCheck = make([]int, 0, len(pool.Cores))
for cid := range pool.Cores {
toCheck = append(toCheck, cid)
}
pool.Mu.RUnlock()
} else {
toCheck = make([]int, numCores)
for j := range toCheck {
toCheck[j] = j
}
}

for _, coreID := range toCheck {
var usedBudget float64
var corePods []PodInfo

if pool != nil {
pool.Mu.RLock()
if core, ok := pool.Cores[coreID]; ok {
usedBudget = float64(core.UsageMillis) / 1000.0
for _, pi := range core.Pods {
corePods = append(corePods, pi)
}
}
pool.Mu.RUnlock()
}

free := RTCapacityPerCore - usedBudget

if free >= requiredBudget {
direct = append(direct, candidate{
nodeName: nodeName,
coreID:   coreID,
free:     free,
})
} else {
evictCands, evictBudget := poolEvictionLookahead(corePods, incomingCriticality)
if free+evictBudget >= requiredBudget {
min := greedyMinVictims(evictCands, requiredBudget-free)
withEvict = append(withEvict, candidate{
nodeName: nodeName,
coreID:   coreID,
free:     free,
victims:  min,
})
}
}
}
}

// Worst Fit: prefer core with most remaining free budget.
if len(direct) > 0 {
sort.Slice(direct, func(i, j int) bool {
return direct[i].free > direct[j].free
})
b := direct[0]
return FeasibilityResult{
Feasible:         true,
SelectedNodeName: b.nodeName,
SelectedCoreID:   b.coreID,
}
}

// Fallback: eviction required; minimise victim count, break ties by free budget.
if len(withEvict) > 0 {
sort.Slice(withEvict, func(i, j int) bool {
li, lj := len(withEvict[i].victims), len(withEvict[j].victims)
if li != lj {
return li < lj
}
return withEvict[i].free > withEvict[j].free
})
b := withEvict[0]
return FeasibilityResult{
Feasible:         true,
SelectedNodeName: b.nodeName,
SelectedCoreID:   b.coreID,
VictimPodNames:   b.victims,
}
}

return FeasibilityResult{Feasible: false}
}

// CheckNodeCoreFeasibility checks whether a specific (node, core) pair can
// accommodate a new RT pod, optionally via eviction of lower-criticality pods.
// Used by the Validating Webhook after the Mutating Webhook has fixed the core.
//
// If the node pool does not yet exist the core is fully free → (true, nil).
func CheckNodeCoreFeasibility(
nodeName string,
coreID int,
requiredBudget float64,
incomingCriticality string,
) (feasible bool, victims []string) {
globalPoolsMu.RLock()
pool := globalPools[nodeName]
globalPoolsMu.RUnlock()

if pool == nil {
return true, nil // node not yet in pool → completely free
}

pool.Mu.RLock()
var usedBudget float64
var corePods []PodInfo
if core, ok := pool.Cores[coreID]; ok {
usedBudget = float64(core.UsageMillis) / 1000.0
for _, pi := range core.Pods {
corePods = append(corePods, pi)
}
}
pool.Mu.RUnlock()

free := RTCapacityPerCore - usedBudget
if free >= requiredBudget {
return true, nil
}

evictCands, total := poolEvictionLookahead(corePods, incomingCriticality)
if free+total >= requiredBudget {
return true, greedyMinVictims(evictCands, requiredBudget-free)
}

return false, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// poolEvictionLookahead returns pods on a core evictable by incomingCriticality
// and their combined budget as a fraction of one core (0.0–1.0).
func poolEvictionLookahead(pods []PodInfo, incomingCriticality string) (victims []PodInfo, total float64) {
rank := CriticalityRank[incomingCriticality]
for _, pod := range pods {
if CriticalityRank[pod.Criticality] < rank {
victims = append(victims, pod)
total += float64(pod.CPUMillis) / 1000.0
}
}
return
}

// SelectMinimumVictims selects the minimum set of PodInfo entries whose combined
// budget covers needed.  Picks lowest criticality first; ties broken by largest budget.
// This is the PodInfo-returning variant used by the Controller for actual preemption.
func SelectMinimumVictims(candidates []PodInfo, needed float64) []PodInfo {
	sorted := make([]PodInfo, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		ri := CriticalityRank[sorted[i].Criticality]
		rj := CriticalityRank[sorted[j].Criticality]
		if ri != rj {
			return ri < rj
		}
		return sorted[i].CPUMillis > sorted[j].CPUMillis
	})

	var result []PodInfo
	acc := 0.0
	for _, c := range sorted {
		result = append(result, c)
		acc += float64(c.CPUMillis) / 1000.0
		if acc >= needed {
			break
		}
	}
	return result
}

// greedyMinVictims selects the minimum set of victim pods whose combined budget
// covers needed.  Picks lowest criticality first; ties broken by largest budget.
func greedyMinVictims(candidates []PodInfo, needed float64) []string {
sort.Slice(candidates, func(i, j int) bool {
ri := CriticalityRank[candidates[i].Criticality]
rj := CriticalityRank[candidates[j].Criticality]
if ri != rj {
return ri < rj
}
return candidates[i].CPUMillis > candidates[j].CPUMillis
})

var result []string
acc := 0.0
for _, c := range candidates {
result = append(result, c.Name)
acc += float64(c.CPUMillis) / 1000.0
if acc >= needed {
break
}
}
return result
}

func filterValidCores(preferred []int, numCores int) []int {
var valid []int
for _, c := range preferred {
if c >= 0 && c < numCores {
valid = append(valid, c)
}
}
return valid
}

// ParseCoreSet parses a core range/list string into a slice of core IDs.
// Supports: "2-3" → [2,3], "1" → [1], "0,2,4" → [0,2,4], "" → [].
func ParseCoreSet(coreStr string) []int {
cores := make([]int, 0)
if coreStr == "" {
return cores
}

parts := strings.Split(coreStr, ",")
for _, part := range parts {
part = strings.TrimSpace(part)
if strings.Contains(part, "-") {
rangeParts := strings.Split(part, "-")
if len(rangeParts) == 2 {
start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
if err1 == nil && err2 == nil {
for i := start; i <= end; i++ {
cores = append(cores, i)
}
}
}
} else {
var coreID int
if _, err := fmt.Sscanf(part, "%d", &coreID); err == nil {
cores = append(cores, coreID)
}
}
}
return cores
}
