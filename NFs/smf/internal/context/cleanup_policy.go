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
	// key: 子網前綴 (如 "10.60.0", "10.61.0")
	// value: 閒置超時時間
	subnetPolicies map[string]time.Duration

	// 預設的閒置超時時間（當沒有匹配的子網策略時使用）
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
			defaultIdleTimeout: 1 * time.Hour, // 預設 1 小時
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
// subnet: 子網前綴，如 "10.60.0" 或 "10.60.0.0/24"
// timeout: 該子網的閒置超時時間
func (p *CleanupPolicy) AddSubnetPolicy(subnet string, timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 嘗試解析 CIDR，如果失敗則當作前綴處理
	prefix := subnet
	if _, ipNet, err := net.ParseCIDR(subnet); err == nil {
		// 提取網路前綴
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

	// 嘗試匹配子網策略
	// 策略 1: 精確匹配前三個 octet (如 "10.60.0")
	prefix := getSubnetPrefix(ip)
	if timeout, ok := p.subnetPolicies[prefix]; ok {
		return timeout
	}

	// 策略 2: 匹配前兩個 octet (如 "10.60")
	prefix2 := getTwoOctetPrefix(ip)
	if timeout, ok := p.subnetPolicies[prefix2]; ok {
		return timeout
	}

	// 沒有匹配的策略，返回預設值
	return p.defaultIdleTimeout
}

// GetAllPolicies 獲取所有策略（用於調試/顯示）
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
// 例如: 10.60.0.5 -> "10.60.0"
func getSubnetPrefix(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return net.IPv4(ip4[0], ip4[1], ip4[2], 0).String()[:len(net.IPv4(ip4[0], ip4[1], ip4[2], 0).String())-2]
}

// getTwoOctetPrefix 獲取 IP 的前兩個 octet 作為子網前綴
// 例如: 10.60.0.5 -> "10.60"
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
// 用於更精細的策略控制，例如 10.60.0.1-10 使用策略 A，10.60.0.11-20 使用策略 B
func MatchLastOctet(ip net.IP, start, end byte) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	lastOctet := ip4[3]
	return lastOctet >= start && lastOctet <= end
}
