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

	// 流量查詢回調函數（由 processor 層設定，用於主動查詢 URR 流量）
	// 返回值: (totalVolume, totalPktNum, error)
	queryTrafficCallback func(*SMContext) (uint64, uint64, error)

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

// SetQueryTrafficCallback 設定流量查詢回調函數
// 此回調函數會在清理前被調用，用於主動查詢 UPF 的流量統計
// 如果查詢到有流量，會更新 LastActiveTime
func (c *SessionCleaner) SetQueryTrafficCallback(callback func(*SMContext) (uint64, uint64, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queryTrafficCallback = callback
	logger.CtxLog.Info("Session cleaner: query traffic callback set")
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

// IsRunning 檢查是否正在運行
func (c *SessionCleaner) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}

// GetStats 獲取統計資料
func (c *SessionCleaner) GetStats() CleanerStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// cleanerLoop 清理器主循環
func (c *SessionCleaner) cleanerLoop() {
	c.mu.RLock()
	interval := c.scanInterval
	c.mu.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			c.scan()
		}
	}
}

// scan 執行一次掃描
func (c *SessionCleaner) scan() {
	policy := GetCleanupPolicy()
	if !policy.IsEnabled() {
		return
	}

	c.mu.RLock()
	callback := c.releaseCallback
	queryTraffic := c.queryTrafficCallback
	c.mu.RUnlock()

	if callback == nil {
		logger.CtxLog.Warn("Session cleaner: release callback not set")
		return
	}

	now := time.Now()
	var potentialIdleSessions []*SMContext

	// 遍歷所有 SM Context，找出可能閒置的 Session
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
		// 條件：CurrentTime - LastActiveTime > IdleTimeout
		if smContext.IdleTimeout > 0 && !smContext.LastActiveTime.IsZero() {
			idleDuration := now.Sub(smContext.LastActiveTime)
			if idleDuration > smContext.IdleTimeout {
				potentialIdleSessions = append(potentialIdleSessions, smContext)
			}
		}
		return true
	})

	// 對於可能閒置的 Session，先查詢流量，確認是否真的閒置
	var confirmedIdleSessions []*SMContext
	for _, smContext := range potentialIdleSessions {
		// 如果設定了流量查詢回調，先查詢最新流量
		if queryTraffic != nil {
			totalVolume, totalPktNum, err := queryTraffic(smContext)
			if err != nil {
				logger.CtxLog.Warnf("[FastCleanup] Failed to query traffic for UE[%s] PDUSessionID[%d]: %v",
					smContext.Supi, smContext.PDUSessionID, err)
				// 查詢失敗時，不清理該 session，等下次再試
				continue
			}

			// 檢查累計流量是否有增加
			lastTotal := smContext.LastReportedVolume + smContext.LastReportedPktNum
			currentTotal := totalVolume + totalPktNum

			logger.CtxLog.Debugf("[FastCleanup] UE[%s] PDUSessionID[%d] traffic check: "+
				"lastTotal=%d, currentTotal=%d, volume=%d, pktNum=%d",
				smContext.Supi, smContext.PDUSessionID, lastTotal, currentTotal, totalVolume, totalPktNum)

			if currentTotal > lastTotal {
				// 有新流量，更新 LastActiveTime 和記錄的流量
				smContext.LastActiveTime = time.Now()
				smContext.LastReportedVolume = totalVolume
				smContext.LastReportedPktNum = totalPktNum
				logger.CtxLog.Infof("[FastCleanup] UE[%s] PDUSessionID[%d] has traffic, updated LastActiveTime",
					smContext.Supi, smContext.PDUSessionID)
				continue // 不加入清理列表
			}
		}

		// 確認是閒置的，加入清理列表
		confirmedIdleSessions = append(confirmedIdleSessions, smContext)
	}

	// 釋放確認閒置的 Session
	cleanedCount := 0
	for _, smContext := range confirmedIdleSessions {
		logger.CtxLog.Infof("[FastCleanup] Releasing idle session: UE[%s] PDUSessionID[%d]",
			smContext.Supi, smContext.PDUSessionID)
		if err := callback(smContext); err != nil {
			logger.CtxLog.Errorf("[FastCleanup] Failed to release session: %v", err)
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
	policy := GetCleanupPolicy()
	if !policy.IsEnabled() {
		// Fast Cleanup is disabled
	}

	c.mu.RLock()
	callback := c.releaseCallback
	c.mu.RUnlock()

	if callback == nil {
		logger.CtxLog.Error("Session cleaner: release callback not set")
		return 0
	}

	now := time.Now()
	cleanedCount := 0

	smContextPool.Range(func(key, value interface{}) bool {
		if value == nil {
			return true
		}
		smContext, ok := value.(*SMContext)
		if !ok || smContext == nil {
			return true
		}

		if smContext.State() != Active {
			return true
		}

		if smContext.IdleTimeout > 0 && !smContext.LastActiveTime.IsZero() {
			idleDuration := now.Sub(smContext.LastActiveTime)
			if idleDuration > smContext.IdleTimeout {
				// 呼叫回調函數進行清理
				if err := callback(smContext); err != nil {
					// 錯誤已記錄
				} else {
					cleanedCount++
				}
			}
		}
		return true
	})

	return cleanedCount
}

// UpdateLastActiveTime 更新 SMContext 的最後活動時間
// 應該在有資料傳輸時呼叫此函數
func (smContext *SMContext) UpdateLastActiveTime() {
	smContext.LastActiveTime = time.Now()
}

// SetIdleTimeoutByPolicy 根據策略設定 IdleTimeout
// 通常在 PDU Session 建立時呼叫
func (smContext *SMContext) SetIdleTimeoutByPolicy() {
	policy := GetCleanupPolicy()
	if !policy.IsEnabled() {
		// Fast Cleanup 未啟用，不設定超時
		smContext.IdleTimeout = 0
		return
	}

	smContext.IdleTimeout = policy.GetIdleTimeoutForIP(smContext.PDUAddress)
	smContext.LastActiveTime = time.Now()

	// Log 設定的超時值
	if smContext.PDUAddress != nil {
		logger.CtxLog.Infof("Set idle timeout for UE[%s] PDUSessionID[%d] IP[%s]: timeout=%v",
			smContext.Supi, smContext.PDUSessionID, smContext.PDUAddress.String(), smContext.IdleTimeout)
	} else {
		logger.CtxLog.Infof("Set idle timeout for UE[%s] PDUSessionID[%d]: timeout=%v (no IP assigned)",
			smContext.Supi, smContext.PDUSessionID, smContext.IdleTimeout)
	}
}

// GetIdleInfo 獲取閒置資訊（用於調試/監控）
func (smContext *SMContext) GetIdleInfo() map[string]interface{} {
	now := time.Now()

	// 安全獲取 IP 字串
	ipStr := ""
	if smContext.PDUAddress != nil {
		ipStr = smContext.PDUAddress.String()
	}

	// 安全獲取 lastActiveTime 字串
	lastActiveStr := ""
	if !smContext.LastActiveTime.IsZero() {
		lastActiveStr = smContext.LastActiveTime.Format(time.RFC3339)
	}

	// 使用 uint32 值而不是 String() 來避免潛在的 nil 問題
	stateValue := smContext.State()

	info := map[string]interface{}{
		"supi":           smContext.Supi,
		"pduSessionId":   smContext.PDUSessionID,
		"ip":             ipStr,
		"idleTimeout":    smContext.IdleTimeout.String(),
		"lastActiveTime": lastActiveStr,
		"state":          stateValue.String(),
	}

	if !smContext.LastActiveTime.IsZero() {
		idleDuration := now.Sub(smContext.LastActiveTime)
		info["idleDuration"] = idleDuration.Round(time.Second).String()
		info["isIdle"] = smContext.IdleTimeout > 0 && idleDuration > smContext.IdleTimeout
		if smContext.IdleTimeout > 0 {
			remaining := smContext.IdleTimeout - idleDuration
			if remaining > 0 {
				info["remainingTime"] = remaining.Round(time.Second).String()
			} else {
				info["remainingTime"] = "expired"
			}
		}
	}

	return info
}

// GetAllSessionsIdleInfo 獲取所有 Session 的閒置資訊
func GetAllSessionsIdleInfo() []map[string]interface{} {
	var infos []map[string]interface{}

	smContextPool.Range(func(key, value interface{}) bool {
		smContext := value.(*SMContext)
		if smContext.State() == Active {
			infos = append(infos, smContext.GetIdleInfo())
		}
		return true
	})

	return infos
}
