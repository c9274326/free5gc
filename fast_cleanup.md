# Fast PDU Session Cleanup 功能文件

## 概述

Fast PDU Session Cleanup 是一個用於自動清理閒置 PDU Session 的功能。當 UE 的 PDU Session 長時間沒有流量活動時，SMF 會自動釋放該 Session，以節省系統資源並維持系統健康狀態。

## 功能特點

- **基於流量的閒置檢測**：透過 UPF 的 URR (Usage Reporting Rule) 回報來偵測實際流量
- **可配置的策略**：支援全域預設超時和基於子網的差異化超時策略
- **非侵入式設計**：僅在有實際流量時才更新 LastActiveTime
- **安全的 Session 釋放**：透過回調函數機制確保正確的資源清理

---

## 修改的檔案總覽

| 檔案路徑 | 修改類型 | 說明 |
|---------|---------|------|
| `internal/context/pfcp_reports.go` | 修改 | 新增流量檢測函數 |
| `internal/context/session_cleaner.go` | 新增 | Session 清理器完整實作 |
| `internal/context/cleanup_policy.go` | 新增 | 清理策略管理 |
| `internal/context/sm_context.go` | 修改 | 新增欄位與函數 |
| `internal/sbi/server.go` | 修改 | 初始化 Fast Cleanup |
| `internal/sbi/processor/pdu_session.go` | 修改 | PDU Session 建立時設定 IdleTimeout |
| `internal/sbi/processor/resource_release.go` | 新增 | 網路發起釋放功能 |
| `pkg/factory/config.go` | 修改 | 新增配置結構 |
| `internal/context/fast_cleanup_test.go` | 新增 | 完整測試套件 |

---

## 檔案詳細改動

### 1. `internal/context/pfcp_reports.go`

#### 新增函數：`hasActualTraffic()`

檢查 URR 回報中是否包含實際流量。

```go
// hasActualTraffic 檢查 Usage Report 中是否有實際的資料傳輸
// 只有當 TotalVolume > 0 或封包數 > 0 時才認為有活動
func hasActualTraffic(
	usageReportRequest []*pfcp.UsageReportPFCPSessionReportRequest,
	usageReportModification []*pfcp.UsageReportPFCPSessionModificationResponse,
) bool {
	for _, report := range usageReportRequest {
		if report.VolumeMeasurement != nil {
			if report.VolumeMeasurement.TotalVolume > 0 ||
				report.VolumeMeasurement.TotalPktNum > 0 {
				return true
			}
		}
	}
	for _, report := range usageReportModification {
		if report.VolumeMeasurement != nil {
			if report.VolumeMeasurement.TotalVolume > 0 ||
				report.VolumeMeasurement.TotalPktNum > 0 {
				return true
			}
		}
	}
	return false
}
```

#### 修改函數：`HandleReports()`

在處理 URR 回報時，只有當偵測到實際流量時才更新 `LastActiveTime`：

```go
func (smContext *SMContext) HandleReports(
	usageReportRequest []*pfcp.UsageReportPFCPSessionReportRequest,
	usageReportModification []*pfcp.UsageReportPFCPSessionModificationResponse,
	usageReportDeletion []*pfcp.UsageReportPFCPSessionDeletionResponse,
	nodeId pfcpType.NodeID, reportTpye models.ChfConvergedChargingTriggerType,
) {
	// Fast Cleanup: 檢查是否有實際資料傳輸，若有則更新最後活動時間
	hasTraffic := hasActualTraffic(usageReportRequest, usageReportModification)
	if hasTraffic {
		smContext.UpdateLastActiveTime()
		logger.PduSessLog.Debugf("Updated LastActiveTime for SM Context %s due to traffic", smContext.Ref)
	}

	// ... 原有的 URR 報告處理邏輯 ...
}
```

---

### 2. `internal/context/session_cleaner.go` (新增檔案)

完整的 Session 清理器實作：

```go
package context

import (
	"sync"
	"time"

	"github.com/free5gc/smf/internal/logger"
)

// SessionCleaner 負責掃描並清理閒置的 PDU Session
type SessionCleaner struct {
	mu sync.RWMutex

	// 掃描間隔
	scanInterval time.Duration

	// 是否正在運行
	running bool

	// 停止信號
	stopChan chan struct{}

	// 釋放回調函數（由 processor 層設定）
	releaseCallback func(*SMContext) error

	// 統計資料
	stats CleanerStats
}

// CleanerStats 清理器統計資料
type CleanerStats struct {
	TotalScans       uint64    // 總掃描次數
	TotalCleaned     uint64    // 總清理數量
	LastScanTime     time.Time // 最後掃描時間
	LastCleanedCount int       // 最後一次清理數量
}

var sessionCleaner *SessionCleaner
var sessionCleanerOnce sync.Once

// GetSessionCleaner 獲取 Session 清理器單例
func GetSessionCleaner() *SessionCleaner {
	sessionCleanerOnce.Do(func() {
		sessionCleaner = &SessionCleaner{
			scanInterval: 30 * time.Second, // 預設 30 秒掃描一次
			running:      false,
			stopChan:     make(chan struct{}),
		}
	})
	return sessionCleaner
}

// SetScanInterval 設定掃描間隔
func (c *SessionCleaner) SetScanInterval(interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scanInterval = interval
	logger.CtxLog.Infof("Session cleaner scan interval set to: %v", interval)
}

// SetReleaseCallback 設定釋放回調函數
func (c *SessionCleaner) SetReleaseCallback(callback func(*SMContext) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCallback = callback
}

// Start 啟動清理器
func (c *SessionCleaner) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		logger.CtxLog.Warn("Session cleaner is already running")
		return
	}
	c.running = true
	c.stopChan = make(chan struct{})
	c.mu.Unlock()

	go c.cleanerLoop()
	logger.CtxLog.Info("Session cleaner started")
}

// Stop 停止清理器
func (c *SessionCleaner) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return
	}

	close(c.stopChan)
	c.running = false
	logger.CtxLog.Info("Session cleaner stopped")
}

// scan 執行一次掃描
func (c *SessionCleaner) scan() {
	policy := GetCleanupPolicy()
	if !policy.IsEnabled() {
		return
	}

	c.mu.RLock()
	callback := c.releaseCallback
	c.mu.RUnlock()

	if callback == nil {
		logger.CtxLog.Warn("Session cleaner: release callback not set")
		return
	}

	now := time.Now()
	var idleSessions []*SMContext

	// 遍歷所有 SM Context，找出閒置的 Session
	smContextPool.Range(func(key, value interface{}) bool {
		if value == nil {
			return true
		}
		smContext, ok := value.(*SMContext)
		if !ok || smContext == nil {
			return true
		}

		// 只檢查 Active 狀態的 Session
		if smContext.State() != Active {
			return true
		}

		// 檢查是否超過閒置時間
		if smContext.IdleTimeout > 0 && !smContext.LastActiveTime.IsZero() {
			idleDuration := now.Sub(smContext.LastActiveTime)
			if idleDuration > smContext.IdleTimeout {
				idleSessions = append(idleSessions, smContext)
			}
		}
		return true
	})

	// 釋放閒置的 Session
	cleanedCount := 0
	for _, smContext := range idleSessions {
		if err := callback(smContext); err != nil {
			// 錯誤處理
		} else {
			cleanedCount++
		}
	}

	// 更新統計資料
	c.mu.Lock()
	c.stats.TotalScans++
	c.stats.TotalCleaned += uint64(cleanedCount)
	c.stats.LastScanTime = now
	c.stats.LastCleanedCount = cleanedCount
	c.mu.Unlock()
}

// ForceCleanup 強制執行一次清理（不等待定時器）
func (c *SessionCleaner) ForceCleanup() int {
	// ... 與 scan() 類似但直接返回清理數量 ...
}

// UpdateLastActiveTime 更新 SMContext 的最後活動時間
func (smContext *SMContext) UpdateLastActiveTime() {
	smContext.LastActiveTime = time.Now()
}

// SetIdleTimeoutByPolicy 根據策略設定 IdleTimeout
func (smContext *SMContext) SetIdleTimeoutByPolicy() {
	policy := GetCleanupPolicy()
	if !policy.IsEnabled() {
		smContext.IdleTimeout = 0
		return
	}

	smContext.IdleTimeout = policy.GetIdleTimeoutForIP(smContext.PDUAddress)
	smContext.LastActiveTime = time.Now()

	if smContext.PDUAddress != nil {
		logger.CtxLog.Infof("Set idle timeout for UE[%s] PDUSessionID[%d] IP[%s]: timeout=%v",
			smContext.Supi, smContext.PDUSessionID, smContext.PDUAddress.String(), smContext.IdleTimeout)
	}
}

// GetIdleInfo 獲取閒置資訊（用於調試/監控）
func (smContext *SMContext) GetIdleInfo() map[string]interface{} {
	// ... 返回 Session 的閒置狀態資訊 ...
}

// GetAllSessionsIdleInfo 獲取所有 Session 的閒置資訊
func GetAllSessionsIdleInfo() []map[string]interface{} {
	// ... 遍歷所有 Active Session 並收集閒置資訊 ...
}
```

---

### 3. `internal/context/cleanup_policy.go` (新增檔案)

完整的清理策略管理：

```go
package context

import (
	"net"
	"sync"
	"time"

	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/pkg/factory"
)

// CleanupPolicy 定義 PDU Session 清理策略
type CleanupPolicy struct {
	mu sync.RWMutex

	// 子網到 IdleTimeout 的映射
	subnetPolicies map[string]time.Duration

	// 預設的閒置超時時間
	defaultIdleTimeout time.Duration

	// 是否啟用 Fast Cleanup
	enabled bool
}

var cleanupPolicy *CleanupPolicy
var cleanupPolicyOnce sync.Once

// GetCleanupPolicy 獲取清理策略單例
func GetCleanupPolicy() *CleanupPolicy {
	cleanupPolicyOnce.Do(func() {
		cleanupPolicy = &CleanupPolicy{
			subnetPolicies:     make(map[string]time.Duration),
			defaultIdleTimeout: 1 * time.Hour,
			enabled:            false,
		}
	})
	return cleanupPolicy
}

// SetEnabled 啟用/停用 Fast Cleanup
func (p *CleanupPolicy) SetEnabled(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = enabled
	logger.CtxLog.Infof("Fast Cleanup %s", map[bool]string{true: "enabled", false: "disabled"}[enabled])
}

// IsEnabled 檢查是否啟用
func (p *CleanupPolicy) IsEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enabled
}

// SetDefaultIdleTimeout 設定預設閒置超時時間
func (p *CleanupPolicy) SetDefaultIdleTimeout(timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defaultIdleTimeout = timeout
	logger.CtxLog.Infof("Default idle timeout set to: %v", timeout)
}

// GetDefaultIdleTimeout 獲取預設閒置超時時間
func (p *CleanupPolicy) GetDefaultIdleTimeout() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.defaultIdleTimeout
}

// AddSubnetPolicy 新增子網策略
func (p *CleanupPolicy) AddSubnetPolicy(subnet string, timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	prefix := subnet
	if _, ipNet, err := net.ParseCIDR(subnet); err == nil {
		prefix = getSubnetPrefix(ipNet.IP)
	}

	p.subnetPolicies[prefix] = timeout
	logger.CtxLog.Infof("Added cleanup policy: subnet=%s, idle_timeout=%v", prefix, timeout)
}

// RemoveSubnetPolicy 移除子網策略
func (p *CleanupPolicy) RemoveSubnetPolicy(subnet string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.subnetPolicies, subnet)
	logger.CtxLog.Infof("Removed cleanup policy for subnet: %s", subnet)
}

// GetIdleTimeoutForIP 根據 IP 地址獲取對應的閒置超時時間
func (p *CleanupPolicy) GetIdleTimeoutForIP(ip net.IP) time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if ip == nil {
		return p.defaultIdleTimeout
	}

	// 嘗試匹配子網策略 (前三個 octet)
	prefix := getSubnetPrefix(ip)
	if timeout, ok := p.subnetPolicies[prefix]; ok {
		return timeout
	}

	// 嘗試匹配前兩個 octet
	prefix2 := getTwoOctetPrefix(ip)
	if timeout, ok := p.subnetPolicies[prefix2]; ok {
		return timeout
	}

	return p.defaultIdleTimeout
}

// GetAllPolicies 獲取所有策略
func (p *CleanupPolicy) GetAllPolicies() map[string]time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make(map[string]time.Duration)
	for k, v := range p.subnetPolicies {
		result[k] = v
	}
	return result
}

// getSubnetPrefix 獲取 IP 的前三個 octet 作為子網前綴
func getSubnetPrefix(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return net.IPv4(ip4[0], ip4[1], ip4[2], 0).String()[:len(net.IPv4(ip4[0], ip4[1], ip4[2], 0).String())-2]
}

// getTwoOctetPrefix 獲取 IP 的前兩個 octet 作為子網前綴
func getTwoOctetPrefix(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return net.IPv4(ip4[0], ip4[1], 0, 0).String()[:len(net.IPv4(ip4[0], ip4[1], 0, 0).String())-4]
}

// InitFromConfig 從 factory 配置初始化策略
func (p *CleanupPolicy) InitFromConfig(config *factory.FastCleanup) {
	if config == nil {
		return
	}

	p.SetEnabled(config.Enabled)

	if config.DefaultIdleTimeout > 0 {
		p.SetDefaultIdleTimeout(time.Duration(config.DefaultIdleTimeout) * time.Second)
	}

	for _, policy := range config.SubnetPolicies {
		p.AddSubnetPolicy(policy.Subnet, time.Duration(policy.IdleTimeout)*time.Second)
	}
}

// MatchLastOctet 檢查 IP 的最後一個 octet 是否在指定範圍內
func MatchLastOctet(ip net.IP, start, end byte) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	lastOctet := ip4[3]
	return lastOctet >= start && lastOctet <= end
}
```

---

### 4. `internal/context/sm_context.go`

#### 新增欄位

在 `SMContext` 結構中新增：

```go
type SMContext struct {
	// ... 原有欄位 ...

	// ===== Fast Cleanup 新增欄位 =====
	// LastActiveTime 記錄最後一次資料活動時間（用於閒置檢測）
	LastActiveTime time.Time
	// IdleTimeout 閒置超時時間（根據子網策略設定）
	IdleTimeout time.Duration
	// ================================

	// lock
	SMLock sync.Mutex
}
```

#### 新增函數：`DeleteSMContextFromPool()`

```go
// DeleteSMContextFromPool 僅從 pool 中刪除 SMContext（用於測試）
// 注意：此函數不會釋放 UPF 資源，僅供測試使用
func DeleteSMContextFromPool(ref string) {
	smContextPool.Delete(ref)
}
```

---

### 5. `internal/sbi/server.go`

#### 修改：`NewServer()` 函數

在伺服器初始化時初始化 Fast Cleanup：

```go
func NewServer(smf ServerSmf, tlsKeyLogPath string) (*Server, error) {
	s := &Server{
		ServerSmf: smf,
	}

	smf_context.InitSmfContext(factory.SmfConfig)
	smf_context.AllocateUPFID()
	smf_context.InitSMFUERouting(factory.UERoutingConfig)

	// Initialize Fast Cleanup for idle PDU Session management
	if factory.SmfConfig.Configuration != nil {
		s.Processor().InitFastCleanup(factory.SmfConfig.Configuration.FastCleanup)
	}

	s.router = newRouter(s)

	// ... 其餘初始化邏輯 ...
}
```

---

### 6. `internal/sbi/processor/pdu_session.go`

#### 修改：`HandlePDUSessionSMContextCreate()` 函數

在 PDU Session 建立時設定 IdleTimeout：

```go
func (p *Processor) HandlePDUSessionSMContextCreate(
	c *gin.Context,
	request models.PostSmContextsRequest,
	isDone <-chan struct{},
) {
	// ... 原有的 Session 建立邏輯 ...

	// ===== Fast Cleanup: 設定 IdleTimeout =====
	// 在 IP 分配後，根據子網策略設定該 Session 的閒置超時時間
	smContext.SetIdleTimeoutByPolicy()
	// ==========================================

	// ... 其餘處理邏輯 ...
}
```

---

### 7. `internal/sbi/processor/resource_release.go` (新增檔案)

網路發起的 PDU Session 釋放功能：

```go
package processor

import (
	"time"

	"github.com/free5gc/nas/nasMessage"
	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/pkg/factory"
)

// InitFastCleanup 初始化 Fast Cleanup 機制
func (p *Processor) InitFastCleanup(config *factory.FastCleanup) {
	if config == nil {
		logger.MainLog.Info("Fast Cleanup config not found, skipping initialization")
		return
	}

	// 初始化策略
	policy := smf_context.GetCleanupPolicy()
	policy.InitFromConfig(config)

	// 初始化清理器
	cleaner := smf_context.GetSessionCleaner()

	// 設定掃描間隔
	if config.ScanInterval > 0 {
		cleaner.SetScanInterval(time.Duration(config.ScanInterval) * time.Second)
	}

	// 設定釋放回調 - 使用網路發起的釋放
	cleaner.SetReleaseCallback(func(smContext *smf_context.SMContext) error {
		return p.NetworkInitiatedReleaseSession(smContext, nasMessage.Cause5GSMInsufficientResourcesForSpecificSliceAndDNN)
	})

	// 啟動清理器
	if config.Enabled {
		cleaner.Start()
		logger.MainLog.Infof("Fast Cleanup enabled - scan_interval=%ds, default_idle_timeout=%ds",
			config.ScanInterval, config.DefaultIdleTimeout)
	}
}

// NetworkInitiatedReleaseSession 網路發起的 PDU Session 釋放
func (p *Processor) NetworkInitiatedReleaseSession(smContext *smf_context.SMContext, cause uint8) error {
	smContext.SMLock.Lock()
	defer smContext.SMLock.Unlock()

	logger.PduSessLog.Infof("Network initiated release for UE[%s] PDUSessionID[%d], cause: %d",
		smContext.Supi, smContext.PDUSessionID, cause)

	// 檢查狀態是否允許釋放
	state := smContext.State()
	if state != smf_context.Active && state != smf_context.ModificationPending {
		logger.PduSessLog.Warnf("SM Context state [%s] not suitable for release", state)
		return nil
	}

	// 設定狀態為釋放中
	smContext.SetState(smf_context.InActivePending)

	// 呼叫已有的釋放邏輯
	needNotify, removeContext := p.requestAMFToReleasePDUResources(smContext)

	if needNotify {
		p.SendReleaseNotification(smContext)
	}

	if removeContext {
		p.RemoveSMContextFromAllNF(smContext, false)
	}

	return nil
}

// ForceCleanupIdleSessions 強制清理閒置的 Session（用於測試/手動觸發）
func (p *Processor) ForceCleanupIdleSessions() int {
	cleaner := smf_context.GetSessionCleaner()
	return cleaner.ForceCleanup()
}

// GetCleanerStats 獲取清理器統計資料
func (p *Processor) GetCleanerStats() smf_context.CleanerStats {
	cleaner := smf_context.GetSessionCleaner()
	return cleaner.GetStats()
}

// GetAllSessionsIdleInfo 獲取所有 Session 的閒置資訊
func (p *Processor) GetAllSessionsIdleInfo() []map[string]interface{} {
	return smf_context.GetAllSessionsIdleInfo()
}
```

---

### 8. `pkg/factory/config.go`

#### 新增配置結構

```go
type Configuration struct {
	// ... 原有配置 ...
	T3591                *TimerValue          `yaml:"t3591" valid:"required"`
	T3592                *TimerValue          `yaml:"t3592" valid:"required"`
	NwInstFqdnEncoding   bool                 `yaml:"nwInstFqdnEncoding" valid:"type(bool),optional"`
	RequestedUnit        int32                `yaml:"requestedUnit,omitempty" valid:"optional"`
	FastCleanup          *FastCleanup         `yaml:"fastCleanup,omitempty" valid:"optional"`
}

// FastCleanup Fast PDU Session Cleanup 配置
type FastCleanup struct {
	Enabled            bool                  `yaml:"enabled"`            // 是否啟用
	DefaultIdleTimeout int                   `yaml:"defaultIdleTimeout"` // 預設閒置超時秒數
	ScanInterval       int                   `yaml:"scanInterval"`       // 掃描間隔秒數
	SubnetPolicies     []SubnetCleanupPolicy `yaml:"subnetPolicies"`     // 子網策略
}

// SubnetCleanupPolicy 子網清理策略
type SubnetCleanupPolicy struct {
	Subnet      string `yaml:"subnet"`      // 子網，如 "10.60.0" 或 "10.60.0.0/24"
	IdleTimeout int    `yaml:"idleTimeout"` // 閒置超時秒數
}
```

---

## 測試檔案

### `internal/context/fast_cleanup_test.go`

完整的測試套件，涵蓋所有 Fast Cleanup 功能。

#### 測試初始化

```go
func TestMain(m *testing.M) {
    // Initialize TeidGenerator for tests
    context.TeidGenerator = idgenerator.NewGenerator(1, 0x7fffffff)
    os.Exit(m.Run())
}
```

#### 測試分類

| 測試類別 | 測試數量 | 說明 |
|---------|---------|------|
| CleanupPolicy | 9 | 策略配置、啟用/停用、子網策略 |
| SMContext Idle | 2 | Session 閒置時間檢測 |
| hasActualTraffic | 3 | URR 流量偵測邏輯 |
| MatchLastOctet | 2 | IP 最後 octet 匹配 |
| PDU Session Release | 10 | 實際 PDU Session 釋放功能 |
| Edge Case | 4 | 邊界情況與錯誤處理 |
| Integration | 2 (Skip) | 需要完整 SMF 環境的整合測試 |

#### 主要測試案例

##### CleanupPolicy 測試

```go
func TestCleanupPolicy_DefaultValues(t *testing.T)
func TestCleanupPolicy_CustomValues(t *testing.T)
func TestCleanupPolicy_MinimumIdleTimeout(t *testing.T)
func TestCleanupPolicy_LargeIdleTimeout(t *testing.T)
func TestCleanupPolicy_DisabledCleanup(t *testing.T)
func TestCleanupPolicy_SubnetPolicies(t *testing.T)
func TestCleanupPolicy_GetAllPolicies(t *testing.T)
func TestCleanupPolicy_GetIdleTimeoutForNilIP(t *testing.T)
func TestCleanupPolicy_ToggleEnabled(t *testing.T)
```

##### hasActualTraffic 測試

```go
func TestHasActualTraffic_WithVolume(t *testing.T)    // TotalVolume > 0
func TestHasActualTraffic_WithPackets(t *testing.T)   // TotalPktNum > 0
func TestHasActualTraffic_NoTraffic(t *testing.T)     // 無流量
```

##### PDU Session Release 測試

```go
func TestPDUSessionRelease_SessionRemovedFromPool(t *testing.T)
func TestPDUSessionRelease_MultipleSessionsSameUE(t *testing.T)
func TestPDUSessionRelease_DeleteNonExistentSession(t *testing.T)
func TestPDUSessionRelease_DoubleDelete(t *testing.T)
func TestPDUSessionRelease_ConcurrentDelete(t *testing.T)
func TestPDUSessionRelease_ForceCleanup_WithCallback(t *testing.T)
func TestPDUSessionRelease_ForceCleanup_ActiveSessionNotRemoved(t *testing.T)
func TestPDUSessionRelease_BatchCleanup(t *testing.T)
func TestPDUSessionRelease_VerifySessionState(t *testing.T)
```

##### Edge Case 測試

```go
func TestEdgeCase_EmptySMContextPool(t *testing.T)   // 空 pool 不 panic
func TestEdgeCase_NilCallback(t *testing.T)           // nil callback 處理
func TestEdgeCase_VeryOldSession(t *testing.T)        // 極舊的 session
func TestEdgeCase_ConcurrentCleanup(t *testing.T)     // 並發清理
```

---

## 執行測試

```bash
cd /home/dbgr/free5gc/NFs/smf
go test -v -run "Cleanup|Idle|Traffic|MatchLastOctet|EdgeCase|PDUSessionRelease" ./internal/context/...
```

### 測試結果

```
=== 測試統計 ===
總測試數: 31
通過: 29
跳過: 2 (需要完整 SMF/PFCP 初始化)
失敗: 0

PASS
ok  github.com/free5gc/smf/internal/context
```

---

## 配置說明

### 配置範例 (smfcfg.yaml)

```yaml
fastCleanup:
  enabled: true
  defaultIdleTimeout: 300  # 預設 5 分鐘 (秒)
  scanInterval: 30         # 每 30 秒掃描一次
  subnetPolicies:
    - subnet: "10.60.0.0/24"
      idleTimeout: 120     # 此子網 2 分鐘
    - subnet: "10.61.0.0/24"
      idleTimeout: 600     # 此子網 10 分鐘
```

### 關鍵參數

| 參數 | 類型 | 說明 |
|-----|------|------|
| `enabled` | bool | 是否啟用 Fast Cleanup |
| `defaultIdleTimeout` | int | 預設閒置超時時間（秒） |
| `scanInterval` | int | 掃描閒置 Session 的間隔（秒），預設 30 秒 |
| `subnetPolicies` | array | 子網特定策略列表 |
| `subnetPolicies[].subnet` | string | 子網 CIDR 或前綴 |
| `subnetPolicies[].idleTimeout` | int | 該子網的閒置超時（秒） |

---

## 運作原理

```
┌─────────┐     URR Report      ┌─────────┐
│   UPF   │ ──────────────────▶ │   SMF   │
└─────────┘                     └────┬────┘
                                     │
                                     ▼
                          ┌──────────────────────┐
                          │  hasActualTraffic()  │
                          │  TotalVolume > 0 ?   │
                          │  TotalPktNum > 0 ?   │
                          └──────────┬───────────┘
                                     │
                    ┌────────────────┴────────────────┐
                    │                                 │
                    ▼                                 ▼
           ┌───────────────┐                ┌────────────────┐
           │ 有流量: 更新   │                │ 無流量: 不更新  │
           │ LastActiveTime│                │ LastActiveTime │
           └───────────────┘                └────────────────┘
                                                    │
                                                    ▼
                                         ┌──────────────────┐
                                         │ SessionCleaner   │
                                         │ 定期掃描檢查     │
                                         │ 是否超過 Timeout │
                                         └────────┬─────────┘
                                                  │
                                                  ▼
                                         ┌──────────────────┐
                                         │ 觸發 Callback    │
                                         │ 釋放 PDU Session │
                                         └──────────────────┘
```

---

## 注意事項

1. **測試位置限制**：由於 Go 語言的 `internal` 套件存取限制，測試檔案必須位於 `internal/context/` 目錄下

2. **IdleTimeout 設定**：Session 必須設定 `IdleTimeout > 0` 才會被 ForceCleanup 處理

3. **State 要求**：只有處於 `Active` 狀態的 Session 才會被清理掃描處理

4. **Callback 必要性**：必須透過 `SetReleaseCallback()` 設定回調函數，否則不會執行清理

---

## 版本資訊

- **建立日期**：2025-01-29
- **適用版本**：free5gc SMF
- **測試狀態**：29/31 通過，2 跳過

---

## API 介面

### Processor API

| 函數 | 說明 |
|-----|------|
| `InitFastCleanup(config)` | 初始化 Fast Cleanup 機制 |
| `NetworkInitiatedReleaseSession(ctx, cause)` | 網路發起 PDU Session 釋放 |
| `ForceCleanupIdleSessions()` | 強制執行一次閒置 Session 清理 |
| `GetCleanerStats()` | 獲取清理器統計資料 |
| `GetAllSessionsIdleInfo()` | 獲取所有 Session 的閒置資訊 |

### Context API

| 函數 | 說明 |
|-----|------|
| `GetCleanupPolicy()` | 獲取清理策略單例 |
| `GetSessionCleaner()` | 獲取 Session 清理器單例 |
| `UpdateLastActiveTime()` | 更新 SMContext 的最後活動時間 |
| `SetIdleTimeoutByPolicy()` | 根據策略設定 IdleTimeout |
| `GetIdleInfo()` | 獲取 Session 的閒置資訊 |
| `DeleteSMContextFromPool(ref)` | 從 pool 中刪除 SMContext（測試用）|

---

## 檔案結構

```
free5gc/NFs/smf/
├── internal/
│   ├── context/
│   │   ├── cleanup_policy.go      # 清理策略管理
│   │   ├── session_cleaner.go     # Session 清理器
│   │   ├── pfcp_reports.go        # PFCP 報告處理（修改）
│   │   ├── sm_context.go          # SM Context（修改）
│   │   └── fast_cleanup_test.go   # 測試套件
│   └── sbi/
│       ├── server.go              # 伺服器初始化（修改）
│       └── processor/
│           ├── pdu_session.go     # PDU Session 處理（修改）
│           └── resource_release.go # 資源釋放（新增）
└── pkg/
    └── factory/
        └── config.go              # 配置結構（修改）
```

---

## Fix LastActiveTime

本章節說明如何修復 `LastActiveTime` 無法正確更新的問題。當 UE 有實際流量時，`hasActualTraffic()` 仍然返回 `false`，導致 PDU Session 在連線後的固定時間被清理，而不是在沒有流量後的固定時間被清理。

### 問題現象

1. UE 透過 ping 測試確認有流量通過
2. SMF 日誌顯示 `hasActualTraffic=false`
3. Usage Report 中的 `TotalVolume=0`, `TotalPktNum=0`
4. PDU Session 在 `IdleTimeout` 後被清理，即使 UE 一直有流量

### 問題根因分析

經過深入調查，發現問題涉及多個層面：

#### 1. gtp5g 內核模組的計數器邏輯問題

在 `gtp5g/src/gtpu/encap.c` 中，`update_urr_counter_and_send_report()` 函式的邏輯如下：

```c
// Calculate Volume measurement for each trigger
if (urr->trigger & URR_RPT_TRIGGER_VOLTH) {
    update_counter(&urr->vol_th, volume, uplink, mnop);
    if (check_counter(&urr->vol_th, &urr->volumethreshold)) {
        triggers[report_num] = USAR_TRIGGER_VOLTH;
        urrs[report_num++] = urr;
    }
} else {
    if (urr->period == 0) {
        continue;
    }
    update_period_vol_counter(urr, volume, uplink, mnop);
}
```

**關鍵問題**：當 `VOLTH`（Volume Threshold）觸發器被設置時，gtp5g **只會**更新 `vol_th` 計數器，而**不會**更新 period volume counter（vol1/vol2）。

這意味著：
- 當同時設置 `PERIO` 和 `VOLTH` 時，封包經過時只更新 `vol_th`
- UPF 的 periodic timer 查詢的是 period volume counter
- 因此 PERIO report 的 volume 總是 0

#### 2. SMF 的 `NewVolumeThreshold()` 無條件設置 VOLTH

在 `internal/context/pfcp_rules.go` 中，原始程式碼：

```go
func NewVolumeThreshold(threshold uint64) UrrOpt {
    return func(urr *URR) {
        urr.ReportingTrigger.Volth = true  // 無條件設置！
        urr.VolumeThreshold = threshold
    }
}
```

即使 `threshold = 0`（配置中未設置 urrThreshold），`Volth` 仍會被設為 `true`。

#### 3. UPF 的 MeasurementPeriod 單位轉換問題

在 `NFs/upf/internal/forwarder/gtp5g.go` 中，原始程式碼有一個 TODO 註解：

```go
case ie.MeasurementPeriod:
    measurePeriod, err = i.MeasurementPeriod()
    // ...
    // TODO: convert time.Duration -> ?
    attrs = append(attrs, nl.Attr{
        Type:  gtp5gnl.URR_MEASUREMENT_PERIOD,
        Value: nl.AttrU32(measurePeriod),  // 直接傳入 time.Duration（納秒）
    })
```

`time.Duration` 以納秒為單位，30 秒 = 30,000,000,000 納秒，超過 `uint32` 最大值（4,294,967,295），導致溢出。

---

### 修復方案

#### 修復 1：SMF - `NewVolumeThreshold()` 條件檢查

**檔案**：`NFs/smf/internal/context/pfcp_rules.go`

```go
func NewVolumeThreshold(threshold uint64) UrrOpt {
    return func(urr *URR) {
        // Only set VOLTH trigger when threshold > 0
        // Otherwise gtp5g will only update vol_th counter instead of period counter
        if threshold > 0 {
            urr.ReportingTrigger.Volth = true
            urr.VolumeThreshold = threshold
        }
    }
}

func NewVolumeQuota(quota uint64) UrrOpt {
    return func(urr *URR) {
        // Only set VOLQU trigger when quota > 0
        if quota > 0 {
            urr.ReportingTrigger.Volqu = true
            urr.VolumeQuota = quota
        }
    }
}
```

#### 修復 2：UPF - MeasurementPeriod 單位轉換

**檔案**：`NFs/upf/internal/forwarder/gtp5g.go`

**CreateURR 函式**：

```go
case ie.MeasurementPeriod:
    measurePeriod, err = i.MeasurementPeriod()
    if err != nil {
        return err
    }
    if measurePeriod <= 0 {
        return errors.New("invalid measurement period")
    }
    // Convert time.Duration (nanoseconds) to seconds for gtp5g kernel module
    measurePeriodSec := uint32(measurePeriod / time.Second)
    attrs = append(attrs, nl.Attr{
        Type:  gtp5gnl.URR_MEASUREMENT_PERIOD,
        Value: nl.AttrU32(measurePeriodSec),
    })
```

**UpdateURR 函式**（同樣修改）：

```go
case ie.MeasurementPeriod:
    v, err1 := i.MeasurementPeriod()
    if err1 != nil {
        return nil, err1
    }
    // Convert time.Duration (nanoseconds) to seconds for gtp5g kernel module
    measurePeriodSec := uint32(v / time.Second)
    attrs = append(attrs, nl.Attr{
        Type:  gtp5gnl.URR_MEASUREMENT_PERIOD,
        Value: nl.AttrU32(measurePeriodSec),
    })
```

---

### 配置注意事項

為了讓 Fast Cleanup 正確運作，`smfcfg.yaml` 中的配置應該：

```yaml
urrPeriod: 30 # default usage report period in seconds
# urrThreshold: 500000 # 註解掉或不設置，避免 VOLTH 觸發器被設置
```

**重要**：如果同時設置 `urrPeriod` 和 `urrThreshold`，由於 gtp5g 的限制，period counter 不會被正確更新。建議只使用 `urrPeriod` 來進行流量監測。

---

### 修改的檔案總覽（Fix LastActiveTime）

| 檔案路徑 | 修改類型 | 說明 |
|---------|---------|------|
| `NFs/smf/internal/context/pfcp_rules.go` | 修改 | 條件檢查 threshold > 0 才設置 VOLTH |
| `NFs/upf/internal/forwarder/gtp5g.go` | 修改 | MeasurementPeriod 單位轉換（納秒→秒） |
| `config/smfcfg.yaml` | 修改 | 註解掉 urrThreshold |

---

### 問題流程圖

```
┌─────────────────────────────────────────────────────────────────┐
│                        問題發生流程                              │
└─────────────────────────────────────────────────────────────────┘

SMF 配置
┌─────────────────┐
│ urrPeriod: 30   │
│ urrThreshold: X │ ─────┐
└─────────────────┘      │
                         ▼
                ┌────────────────────┐
                │ NewVolumeThreshold │
                │ Volth = true       │ ◄─── 即使 threshold=0 也設置！
                └────────┬───────────┘
                         │
                         ▼
              ┌──────────────────────┐
              │  URR 同時設置        │
              │  PERIO + VOLTH       │
              └──────────┬───────────┘
                         │
                         ▼
                ┌────────────────────────────────────┐
                │         gtp5g 內核模組              │
                │                                    │
                │  if (VOLTH) {                      │
                │      update vol_th counter ✓       │
                │  } else {                          │
                │      update period counter ✗       │ ◄─── 不會執行！
                │  }                                 │
                └────────────────┬───────────────────┘
                                 │
                                 ▼
                      ┌────────────────────┐
                      │ PERIO Report       │
                      │ TotalVolume = 0    │
                      │ TotalPktNum = 0    │
                      └────────┬───────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │ hasActualTraffic()   │
                    │ returns false        │
                    └──────────┬───────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │ LastActiveTime       │
                    │ 不更新！             │
                    └──────────────────────┘
```

---

### 修復後流程圖

```
┌─────────────────────────────────────────────────────────────────┐
│                        修復後流程                                │
└─────────────────────────────────────────────────────────────────┘

SMF 配置
┌─────────────────┐
│ urrPeriod: 30   │
│ # urrThreshold  │ ─────┐  （已註解）
└─────────────────┘      │
                         ▼
                ┌────────────────────┐
                │ NewVolumeThreshold │
                │ threshold=0        │
                │ → 不設置 Volth     │ ◄─── 修復點 1
                └────────┬───────────┘
                         │
                         ▼
              ┌──────────────────────┐
              │  URR 只設置          │
              │  PERIO (無 VOLTH)    │
              └──────────┬───────────┘
                         │
                         ▼
                ┌────────────────────────────────────┐
                │         gtp5g 內核模組              │
                │                                    │
                │  if (VOLTH) {                      │
                │      // 不執行                     │
                │  } else {                          │
                │      update period counter ✓       │ ◄─── 正確執行！
                │  }                                 │
                └────────────────┬───────────────────┘
                                 │
                                 ▼
                      ┌────────────────────┐
                      │ PERIO Report       │
                      │ TotalVolume > 0    │
                      │ TotalPktNum > 0    │
                      └────────┬───────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │ hasActualTraffic()   │
                    │ returns true         │
                    └──────────┬───────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │ LastActiveTime       │
                    │ 正確更新 ✓           │
                    └──────────────────────┘
```

---

### 驗證方法

1. **重新編譯 SMF 和 UPF**：
   ```bash
   cd /home/ubuntu/free5gc-final-project
   make smf
   make upf
   ```

2. **確認配置**：
   ```yaml
   # smfcfg.yaml
   urrPeriod: 30
   # urrThreshold: 500000  # 確保已註解
   ```

3. **啟動系統並測試**：
   ```bash
   # 啟動 free5gc
   ./run.sh
   
   # 在 UE 端執行 ping 測試
   ping -I uesimtun0 8.8.8.8
   ```

4. **檢查 SMF 日誌**：
   ```bash
   # 應該看到類似以下日誌
   [FastCleanup] Updated LastActiveTime for SM Context xxx
   ```

---

### 版本資訊

- **修復日期**：2025-12-02
- **適用版本**：free5gc SMF v1.4.0, UPF with gtp5g v0.9.x
- **測試狀態**：已驗證修復有效
