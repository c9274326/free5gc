package processor

import (
	"time"

	"github.com/free5gc/nas/nasMessage"
	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/pkg/factory"
)

// InitFastCleanup 初始化 Fast Cleanup 機制
// 這是專案主要功能：基於閒置時間的 PDU Session 清理
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
// 這是 SMF 主動釋放 PDU Session 的核心函數
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
