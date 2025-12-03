package context

import (
	"fmt"

	"github.com/free5gc/openapi/models"
	"github.com/free5gc/pfcp"
	"github.com/free5gc/pfcp/pfcpType"
	"github.com/free5gc/smf/internal/logger"
)

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

func (smContext *SMContext) HandleReports(
	usageReportRequest []*pfcp.UsageReportPFCPSessionReportRequest,
	usageReportModification []*pfcp.UsageReportPFCPSessionModificationResponse,
	usageReportDeletion []*pfcp.UsageReportPFCPSessionDeletionResponse,
	nodeId pfcpType.NodeID, reportTpye models.ChfConvergedChargingTriggerType,
) {
	// Debug: 記錄收到的 Usage Report
	logger.PduSessLog.Debugf("[FastCleanup] HandleReports called for SM Context %s, "+
		"usageReportRequest=%d, usageReportModification=%d, usageReportDeletion=%d",
		smContext.Ref, len(usageReportRequest), len(usageReportModification), len(usageReportDeletion))

	// Debug: 詳細記錄每個 report 的流量資訊
	for i, report := range usageReportRequest {
		if report.VolumeMeasurement != nil {
			triggerInfo := ""
			if report.UsageReportTrigger != nil {
				triggerInfo = fmt.Sprintf(", Perio=%v, Volth=%v",
					report.UsageReportTrigger.Perio, report.UsageReportTrigger.Volth)
			}
			logger.PduSessLog.Debugf("[FastCleanup] usageReportRequest[%d]: TotalVolume=%d, TotalPktNum=%d, "+
				"UplinkVolume=%d, DownlinkVolume=%d%s",
				i, report.VolumeMeasurement.TotalVolume, report.VolumeMeasurement.TotalPktNum,
				report.VolumeMeasurement.UplinkVolume, report.VolumeMeasurement.DownlinkVolume, triggerInfo)
		} else {
			logger.PduSessLog.Debugf("[FastCleanup] usageReportRequest[%d]: VolumeMeasurement is nil", i)
		}
	}
	for i, report := range usageReportModification {
		if report.VolumeMeasurement != nil {
			logger.PduSessLog.Debugf("[FastCleanup] usageReportModification[%d]: TotalVolume=%d, TotalPktNum=%d, "+
				"UplinkVolume=%d, DownlinkVolume=%d",
				i, report.VolumeMeasurement.TotalVolume, report.VolumeMeasurement.TotalPktNum,
				report.VolumeMeasurement.UplinkVolume, report.VolumeMeasurement.DownlinkVolume)
		} else {
			logger.PduSessLog.Debugf("[FastCleanup] usageReportModification[%d]: VolumeMeasurement is nil", i)
		}
	}

	// Fast Cleanup: 檢查是否有實際資料傳輸，若有則更新最後活動時間
	hasTraffic := hasActualTraffic(usageReportRequest, usageReportModification)
	logger.PduSessLog.Debugf("[FastCleanup] hasActualTraffic=%v for SM Context %s",
		hasTraffic, smContext.Ref)

	// 當收到 PERIO report 時，更新 LastActiveTime
	// 這是因為 gtp5g 在同時設置 VOLTH 和 PERIO 時，只更新 vol_th counter
	// 而 period counter 不會被更新，導致 PERIO report 的 volume 總是為 0
	// 但收到 PERIO report 本身就表示 session 還在被 UPF 處理，因此應該更新活動時間
	if hasTraffic {
		smContext.UpdateLastActiveTime()
		logger.PduSessLog.Infof("[FastCleanup] Updated LastActiveTime for SM Context %s (traffic=%v)",
			smContext.Ref, hasTraffic)
	}

	var usageReport UsageReport
	upf := RetrieveUPFNodeByNodeID(nodeId)
	upfId := upf.UUID()

	for _, report := range usageReportRequest {
		usageReport.UrrId = report.URRID.UrrIdValue
		usageReport.UpfId = upfId
		usageReport.TotalVolume = report.VolumeMeasurement.TotalVolume
		usageReport.UplinkVolume = report.VolumeMeasurement.UplinkVolume
		usageReport.DownlinkVolume = report.VolumeMeasurement.DownlinkVolume
		usageReport.TotalPktNum = report.VolumeMeasurement.TotalPktNum
		usageReport.UplinkPktNum = report.VolumeMeasurement.UplinkPktNum
		usageReport.DownlinkPktNum = report.VolumeMeasurement.DownlinkPktNum
		usageReport.ReportTpye = identityTriggerType(report.UsageReportTrigger)

		if reportTpye != "" {
			usageReport.ReportTpye = reportTpye
		}

		smContext.UrrReports = append(smContext.UrrReports, usageReport)
	}
	for _, report := range usageReportModification {
		usageReport.UrrId = report.URRID.UrrIdValue
		usageReport.UpfId = upfId
		usageReport.TotalVolume = report.VolumeMeasurement.TotalVolume
		usageReport.UplinkVolume = report.VolumeMeasurement.UplinkVolume
		usageReport.DownlinkVolume = report.VolumeMeasurement.DownlinkVolume
		usageReport.TotalPktNum = report.VolumeMeasurement.TotalPktNum
		usageReport.UplinkPktNum = report.VolumeMeasurement.UplinkPktNum
		usageReport.DownlinkPktNum = report.VolumeMeasurement.DownlinkPktNum
		usageReport.ReportTpye = identityTriggerType(report.UsageReportTrigger)

		if reportTpye != "" {
			usageReport.ReportTpye = reportTpye
		}

		smContext.UrrReports = append(smContext.UrrReports, usageReport)
	}
	for _, report := range usageReportDeletion {
		usageReport.UrrId = report.URRID.UrrIdValue
		usageReport.UpfId = upfId
		usageReport.TotalVolume = report.VolumeMeasurement.TotalVolume
		usageReport.UplinkVolume = report.VolumeMeasurement.UplinkVolume
		usageReport.DownlinkVolume = report.VolumeMeasurement.DownlinkVolume
		usageReport.TotalPktNum = report.VolumeMeasurement.TotalPktNum
		usageReport.UplinkPktNum = report.VolumeMeasurement.UplinkPktNum
		usageReport.DownlinkPktNum = report.VolumeMeasurement.DownlinkPktNum
		usageReport.ReportTpye = identityTriggerType(report.UsageReportTrigger)

		if reportTpye != "" {
			usageReport.ReportTpye = reportTpye
		}

		smContext.UrrReports = append(smContext.UrrReports, usageReport)
	}
}

func identityTriggerType(usarTrigger *pfcpType.UsageReportTrigger) models.ChfConvergedChargingTriggerType {
	var trigger models.ChfConvergedChargingTriggerType

	switch {
	case usarTrigger.Volth:
		trigger = models.ChfConvergedChargingTriggerType_QUOTA_THRESHOLD
	case usarTrigger.Volqu:
		trigger = models.ChfConvergedChargingTriggerType_QUOTA_EXHAUSTED
	case usarTrigger.Quvti:
		trigger = models.ChfConvergedChargingTriggerType_VALIDITY_TIME
	case usarTrigger.Start:
		trigger = models.ChfConvergedChargingTriggerType_START_OF_SERVICE_DATA_FLOW
	case usarTrigger.Immer:
		logger.PduSessLog.Trace("Reports Query by SMF, trigger should be filled later")
		return ""
	case usarTrigger.Termr:
		trigger = models.ChfConvergedChargingTriggerType_FINAL
	default:
		logger.PduSessLog.Trace("Report is not a charging trigger")
		return ""
	}

	return trigger
}
