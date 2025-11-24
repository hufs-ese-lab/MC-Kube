# MC-Kube Controller Documentation

이 문서는 `MC-Kube-proto/internal/controller/mckube_controller.go`에 구현된 MC-Kube 컨트롤러의 주요 기능과 구조를 설명합니다.

## 목차
1. [개요](#개요)
2. [핵심 데이터 구조](#핵심-데이터-구조)
3. [주요 기능](#주요-기능)
4. [Reconcile 로직](#reconcile-로직)
5. [선점 (Preemption) 메커니즘](#선점-preemption-메커니즘)
6. [CPU Pool 관리](#cpu-pool-관리)
7. [RT 설정 적용](#rt-설정-적용)
8. [설치 요구사항](#설치-요구사항)

---

## 개요

MC-Kube 컨트롤러는 실시간(Real-Time) 워크로드를 위한 Kubernetes 커스텀 컨트롤러입니다. 주요 기능은 다음과 같습니다:

- **RT 설정 관리**: Pod의 CPU period, runtime 설정
- **CPU 코어 할당**: Pod를 특정 CPU 코어에 할당 및 관리
- **선점 스케줄링**: Criticality 기반 Pod 선점
- **CPU 압박 감지 및 처리**: 노드 CPU 압박 상황 모니터링 및 대응

---

## 핵심 데이터 구조

### McKubeReconciler
```go
type McKubeReconciler struct {
    client.Client
    Scheme         *runtime.Scheme
    DynamicClient  dynamic.Interface
    DataCollector  *ipvs.DataCollector
    PodSpecHandler *ipvs.PodSpecHandler
    EventHandler   *ipvs.EventHandler
}
```

### CPUPool
```go
type CPUPool struct {
    NodeName string
    Cores    map[int]*CPUCoreInfo // coreID -> CPUCoreInfo
    mu       sync.RWMutex
}

type CPUCoreInfo struct {
    CoreID      int
    UsageMillis int64              // 현재 할당된 CPU 사용량 (밀리코어)
    Pods        map[string]PodInfo // 이 코어에 할당된 Pod 정보
}

type PodInfo struct {
    Name        string
    Namespace   string
    Criticality string // "Low", "Middle", "High"
    CPUMillis   int64
    CoreSet     []int
}
```

### Criticality 우선순위
```go
var criticalityRank = map[string]int{
    "Low":    0,
    "Middle": 1,
    "High":   2,
}
```

---

## 주요 기능

### 1. Finalizer 관리

**목적**: McKube 리소스 삭제 시 관련 상태를 정리

```go
const mckubeFinalizer = "mckube.mcoperator.sdv.com/finalizer"
```

**동작**:
- McKube 리소스 생성 시 finalizer 추가
- 삭제 시 Pod 상태 정리 후 finalizer 제거

### 2. CPU Pool 관리

**목적**: 각 노드의 CPU 코어별 사용률 및 할당된 Pod 추적

**주요 함수**:
- `getOrCreateCPUPool(nodeName string, numCores int) *CPUPool`
- `updateCPUPoolForPod(ctx, pod, mckube) error`
- `getCoreUtilization(coreID int) float64`

### 3. 선점 (Preemption)

**목적**: CPU 리소스 부족 시 우선순위가 낮은 Pod를 선점하여 공간 확보

**선점 조건**:
1. **임계값 체크**: 코어 사용률이 90% 초과
2. **Criticality 체크**: 선점자 > 피해자
   - High → Middle, Low 선점 가능
   - Middle → Low 선점 가능

**주요 함수**:
- `checkAndPreemptForPod(ctx, pod, cores, cpuMillis, criticality) error`
- `findPreemptionVictims(pool, coreID, criticality) []PodInfo`
- `preemptPod(ctx, victim, pool, currentCore) error`

### 4. RT 설정 적용

**목적**: Pod 컨테이너에 RT period/runtime 설정 적용

**주요 함수**:
- `applyRTSettingsToContainers(ctx, pod, mckube) error`
- `SendRTRequest(nodeIP string, req CgroupRequest) error`

**RT Daemon 통신**:
```go
type CgroupRequest struct {
    ContainerID string  `json:"container_id"`
    Period      int     `json:"period"`
    Runtime     int     `json:"runtime"`
    Core        *string `json:"core,omitempty"`
}
```

### 5. CPU 압박 처리

**목적**: 노드 CPU 압박 시 Pod의 runtime 조정

**동작**:
- `isCpuBusy=true`: Pod를 `runtime_hi`로 전환
- `isCpuBusy=false`: Pod를 `runtime_low`로 복귀

**주요 함수**:
- `handleCPURecovery(ctx, nodeName)`
- EventHandler의 `HandleNodeCPUPressure(ctx, nodeName)`

### 6. Overrun 이벤트 처리

**목적**: RT 태스크의 deadline miss 이벤트 수신 및 처리

**주요 함수**:
- `StartOverrunListener(port int)`
- `handleOverrunEvent(data OverrunData)`

---

## Reconcile 로직

### Reconcile 흐름도

```
1. McKube 리소스 조회
   ↓
2. Finalizer 처리
   - 삭제 중인가? → Cleanup 후 return
   - Finalizer 없음? → 추가 후 requeue
   ↓
3. PodName 체크
   - 비어있음? → return
   ↓
4. Node 정보 업데이트
   - spec.Node 비어있음? → Pod 조회 후 업데이트, requeue
   ↓
5. RT 설정 처리 (RTSettings가 있는 경우)
   - Pod 조회
   - Pod 상태 확인 (Pending/Running)
   - Runtime 상태 초기화
   - Core 할당 정보 처리
   ↓
6. 선점 체크 및 CPU Pool 업데이트
   - 코어 사용률 계산
   - 90% 초과 시 선점 시도
   - CPU Pool에 Pod 등록
   ↓
7. RT 설정 적용
   - Pod가 Running 상태인 경우
   - 컨테이너에 RT 설정 전송
   ↓
8. CPU 압박 처리
   - Node annotation 체크
   - isCpuBusy 상태 변화 감지
   - 필요 시 runtime 조정
```

### Reconcile 트리거 조건

1. **McKube 리소스 변경**
2. **Pod 변경** (관련 McKube로 전파)
3. **Node 변경** (CPU 압박 상태 변화)

---

## 선점 (Preemption) 메커니즘

### 선점 알고리즘

```go
func checkAndPreemptForPod(...) error {
    for _, coreID := range targetCores {
        currentUsage := pool.getCoreUtilization(coreID)
        afterUsage := currentUsage + float64(cpuMillis)/1000.0
        
        // 90% 임계값 초과 시 선점 시도
        if afterUsage > coreUtilizationThreshold {
            victims := findPreemptionVictims(pool, coreID, criticality)
            for _, victim := range victims {
                preemptPod(ctx, victim, pool, coreID)
            }
        }
    }
}
```

### 선점 예시

**시나리오**: Core 2에 다음 Pod들이 할당됨
- Middle Pod: 40% CPU
- Low Pod: 35% CPU
- **High Pod (새로 추가)**: 25% CPU

**계산**:
- 할당 전: 75%
- 할당 후: 100% > 90% → **선점 발생!**

**결과**:
- High Pod가 Low Pod를 선점
- Low Pod는 다른 코어로 이동 또는 evict

---

## CPU Pool 관리

### CPU 사용률 계산

```go
func (p *CPUPool) getCoreUtilization(coreID int) float64 {
    core := p.Cores[coreID]
    return float64(core.UsageMillis) / 1000.0 // 밀리코어를 코어 단위로 변환
}
```

### Pod 등록/제거

```go
// Pod 추가
func (p *CPUPool) addPodToCore(coreID int, podInfo PodInfo)

// Pod 제거
func (p *CPUPool) removePodFromCore(coreID int, podName string)
```

### 최소 부하 코어 찾기

```go
func (p *CPUPool) findLeastLoadedCore() int {
    // 가장 사용률이 낮은 코어 반환
}
```

---

## RT 설정 적용

### RT Daemon 통신

**Endpoint**: `http://<node-ip>:8080/set-rt`

**요청 형식**:
```json
{
  "container_id": "containerd://abc123...",
  "period": 20000,
  "runtime": 4000,
  "core": "0"
}
```

### Runtime 상태 관리

```go
var podRuntimeState = make(map[string]string) // "low" or "hi"
```

**상태 전환**:
- 초기: `low`
- CPU 압박 시: `low` → `hi`
- CPU 복구 시: `hi` → `low`

---

## 설치 요구사항

### 1. Cert-Manager 설치 (Webhook용)

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.2/cert-manager.yaml
```

### 2. CRD 설치

```bash
make install
```

### 3. 컨트롤러 배포

```bash
make deploy IMG=<your-registry>/mc-kube-controller:latest
```

### 4. RT Daemon 설치 (각 노드)

각 노드에 RT 설정을 적용할 데몬이 실행되어야 합니다.
- Port: 8080
- Endpoint: `/set-rt`

---

## 주요 상수

```go
const targetNamespace = "default"
const rtPressureTaintKey = "McKubeRTDeadlinePressure"
const mckubeFinalizer = "mckube.mcoperator.sdv.com/finalizer"
const coreUtilizationThreshold = 0.9 // 90%
const polling_rate = 10 // 초
```

---

## SetupWithManager

컨트롤러 매니저 설정:

```go
func (r *McKubeReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&mcoperatorv1.McKube{}).
        Watches(&corev1.Pod{}, 
            handler.EnqueueRequestsFromMapFunc(r.findObjectsForPod)).
        Watches(&corev1.Node{}, 
            handler.EnqueueRequestsFromMapFunc(r.EventHandler.FindObjectsForNode)).
        Complete(r)
}
```

**Watch 대상**:
1. McKube 리소스
2. Pod 리소스 (관련 McKube로 이벤트 전파)
3. Node 리소스 (CPU 압박 감지)

---

## 문제 해결

### Pod가 CPU 코어에 할당되지 않는 경우

1. **컨트롤러 로그 확인**:
   ```bash
   kubectl logs -n mc-kube-system -l control-plane=controller-manager
   ```

2. **McKube 리소스 상태 확인**:
   ```bash
   kubectl get mckube <name> -o yaml
   ```
   - `spec.node` 설정 여부
   - `status.allocatedCore` 설정 여부

3. **Pod 상태 확인**:
   ```bash
   kubectl get pod <name> -o yaml
   ```
   - NodeName 할당 여부
   - Phase (Pending/Running)

### 선점이 발생하지 않는 경우

- CPU 사용률이 90% 미만인지 확인
- Criticality 순서 확인 (High > Middle > Low)

---

## 참고

- Webhook 구현: `internal/webhook/pod_mutator.go`
- IPVS 이벤트 처리: `internal/ipvs/event_handler.go`
- API 정의: `api/v1/mckube_types.go`