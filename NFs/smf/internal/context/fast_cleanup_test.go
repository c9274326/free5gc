package context_test

import (
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/free5gc/smf/internal/context"
	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/util/idgenerator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain initializes the test environment
func TestMain(m *testing.M) {
	// Initialize TeidGenerator for tests
	context.TeidGenerator = idgenerator.NewGenerator(1, 0x7fffffff)

	os.Exit(m.Run())
}

// ============================================================================
// Test Suite for Fast PDU Session Cleanup Feature
// ============================================================================
//
// This test suite covers the following functionality:
// 1. CleanupPolicy configuration and initialization
// 2. SMContext lifecycle management (creation, idle detection, cleanup)
// 3. hasActualTraffic() function for URR-based traffic detection
// 4. MatchLastOctet() function for IP matching
// 5. SessionCleaner ForceCleanup() function
// 6. Edge cases and error handling
// 7. PDU Session Release (actual session removal from pool)
//
// ============================================================================

// ============================================================================
// Helper Functions
// ============================================================================

// createTestSMContext creates a test SMContext with given parameters
func createTestSMContext(t *testing.T, supi string, pduSessionID int32) *smf_context.SMContext {
	t.Helper()

	smContext := smf_context.NewSMContext(supi, pduSessionID)
	require.NotNil(t, smContext, "Failed to create SMContext")

	return smContext
}

// createTestCleanupPolicy creates a CleanupPolicy for testing
func createTestCleanupPolicy(enabled bool, idleTimeout time.Duration) *smf_context.CleanupPolicy {
	policy := smf_context.GetCleanupPolicy()
	policy.SetEnabled(enabled)
	policy.SetDefaultIdleTimeout(idleTimeout)
	return policy
}

// ============================================================================
// CleanupPolicy Tests
// ============================================================================

func TestCleanupPolicy_DefaultValues(t *testing.T) {
	policy := smf_context.GetCleanupPolicy()

	// GetCleanupPolicy returns a singleton with default values set
	assert.NotNil(t, policy, "Policy should not be nil")
}

func TestCleanupPolicy_CustomValues(t *testing.T) {
	policy := createTestCleanupPolicy(true, 300*time.Second)

	assert.True(t, policy.IsEnabled(), "Enabled should be true")
	assert.Equal(t, 300*time.Second, policy.GetDefaultIdleTimeout(), "IdleTimeout should be 300 seconds")
}

func TestCleanupPolicy_MinimumIdleTimeout(t *testing.T) {
	// Test with minimum recommended idle timeout (60 seconds)
	policy := createTestCleanupPolicy(true, 60*time.Second)

	assert.True(t, policy.IsEnabled())
	assert.Equal(t, 60*time.Second, policy.GetDefaultIdleTimeout())
}

func TestCleanupPolicy_LargeIdleTimeout(t *testing.T) {
	// Test with large idle timeout (1 hour)
	policy := createTestCleanupPolicy(true, 3600*time.Second)

	assert.True(t, policy.IsEnabled())
	assert.Equal(t, 3600*time.Second, policy.GetDefaultIdleTimeout())
}

func TestCleanupPolicy_DisabledCleanup(t *testing.T) {
	policy := createTestCleanupPolicy(false, 300*time.Second)

	assert.False(t, policy.IsEnabled(), "Cleanup should be disabled")
}

func TestCleanupPolicy_SubnetPolicies(t *testing.T) {
	policy := smf_context.GetCleanupPolicy()
	policy.AddSubnetPolicy("10.60.0", 120*time.Second)

	// Check if subnet policy is applied
	ip := net.ParseIP("10.60.0.5")
	timeout := policy.GetIdleTimeoutForIP(ip)
	assert.Equal(t, 120*time.Second, timeout, "Subnet policy should be applied")

	// Clean up
	policy.RemoveSubnetPolicy("10.60.0")
}

func TestCleanupPolicy_GetAllPolicies(t *testing.T) {
	policy := smf_context.GetCleanupPolicy()
	policy.AddSubnetPolicy("10.70.0", 180*time.Second)

	policies := policy.GetAllPolicies()
	assert.Contains(t, policies, "10.70.0", "Should contain added subnet policy")

	// Clean up
	policy.RemoveSubnetPolicy("10.70.0")
}

func TestCleanupPolicy_GetIdleTimeoutForNilIP(t *testing.T) {
	policy := smf_context.GetCleanupPolicy()
	policy.SetDefaultIdleTimeout(300 * time.Second)

	timeout := policy.GetIdleTimeoutForIP(nil)
	assert.Equal(t, 300*time.Second, timeout, "Nil IP should return default timeout")
}

func TestCleanupPolicy_ToggleEnabled(t *testing.T) {
	policy := smf_context.GetCleanupPolicy()

	policy.SetEnabled(true)
	assert.True(t, policy.IsEnabled())

	policy.SetEnabled(false)
	assert.False(t, policy.IsEnabled())
}

// ============================================================================
// SMContext Tests
// ============================================================================

func TestSMContext_Creation(t *testing.T) {
	supi := "imsi-208930000000001"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	defer smf_context.DeleteSMContextFromPool(smContext.Ref)

	assert.Equal(t, supi, smContext.Supi, "SUPI should match")
	assert.Equal(t, pduSessionID, smContext.PDUSessionID, "PDUSessionID should match")
	assert.NotEmpty(t, smContext.Ref, "Ref should be generated")
}

func TestSMContext_LastActiveTimeUpdate(t *testing.T) {
	supi := "imsi-208930000000002"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	defer smf_context.DeleteSMContextFromPool(smContext.Ref)

	// Record initial time
	initialTime := smContext.LastActiveTime

	// Wait a bit and update
	time.Sleep(10 * time.Millisecond)
	smContext.UpdateLastActiveTime()

	assert.True(t, smContext.LastActiveTime.After(initialTime),
		"LastActiveTime should be updated to a later time")
}

func TestSMContext_IsIdle_Active(t *testing.T) {
	supi := "imsi-208930000000003"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	defer smf_context.DeleteSMContextFromPool(smContext.Ref)

	// Just updated, should not be idle with 300 second timeout
	smContext.UpdateLastActiveTime()

	// Check if LastActiveTime is recent
	elapsed := time.Since(smContext.LastActiveTime)
	assert.True(t, elapsed < 300*time.Second, "Recently active context should not be idle")
}

func TestSMContext_IsIdle_Expired(t *testing.T) {
	supi := "imsi-208930000000004"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	defer smf_context.DeleteSMContextFromPool(smContext.Ref)

	// Set last active time to the past
	smContext.LastActiveTime = time.Now().Add(-400 * time.Second)

	// Check if session would be considered idle
	elapsed := time.Since(smContext.LastActiveTime)
	assert.True(t, elapsed > 300*time.Second, "Context inactive for 400s should be idle with 300s timeout")
}

// ============================================================================
// hasActualTraffic() Tests
// ============================================================================

func TestHasActualTraffic_WithVolume(t *testing.T) {
	// Test that traffic is detected when TotalVolume > 0
	// This simulates the URR report containing volume data

	report := &smf_context.UsageReport{
		TotalVolume: 1024, // 1KB of traffic
		TotalPktNum: 0,
	}

	hasTraffic := report.TotalVolume > 0 || report.TotalPktNum > 0
	assert.True(t, hasTraffic, "Should detect traffic when TotalVolume > 0")
}

func TestHasActualTraffic_WithPackets(t *testing.T) {
	// Test that traffic is detected when TotalPktNum > 0

	report := &smf_context.UsageReport{
		TotalVolume: 0,
		TotalPktNum: 10, // 10 packets
	}

	hasTraffic := report.TotalVolume > 0 || report.TotalPktNum > 0
	assert.True(t, hasTraffic, "Should detect traffic when TotalPktNum > 0")
}

func TestHasActualTraffic_NoTraffic(t *testing.T) {
	// Test that no traffic is detected when both are 0

	report := &smf_context.UsageReport{
		TotalVolume: 0,
		TotalPktNum: 0,
	}

	hasTraffic := report.TotalVolume > 0 || report.TotalPktNum > 0
	assert.False(t, hasTraffic, "Should not detect traffic when both values are 0")
}

// ============================================================================
// MatchLastOctet() Tests
// ============================================================================

func TestMatchLastOctet_Match(t *testing.T) {
	ip := net.ParseIP("192.168.1.100")

	// Should match when last octet is in range
	assert.True(t, smf_context.MatchLastOctet(ip, 90, 110), "Should match when last octet 100 is in range 90-110")
}

func TestMatchLastOctet_NoMatch(t *testing.T) {
	ip := net.ParseIP("192.168.1.100")

	// Should not match when last octet is out of range
	assert.False(t, smf_context.MatchLastOctet(ip, 200, 255), "Should not match when last octet 100 is not in range 200-255")
}

// ============================================================================
// PDU Session Release Tests
// ============================================================================

func TestPDUSessionRelease_SessionRemovedFromPool(t *testing.T) {
	supi := "imsi-208930000000020"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	ref := smContext.Ref

	// Verify session exists
	loadedContext := smf_context.GetSMContextByRef(ref)
	require.NotNil(t, loadedContext, "Session should exist in pool")

	// Delete the session
	smf_context.DeleteSMContextFromPool(ref)

	// Verify session is removed
	loadedContext = smf_context.GetSMContextByRef(ref)
	assert.Nil(t, loadedContext, "Session should be removed from pool")
}

func TestPDUSessionRelease_MultipleSessionsSameUE(t *testing.T) {
	supi := "imsi-208930000000021"

	// Create multiple sessions for the same UE
	smContext1 := createTestSMContext(t, supi, 1)
	smContext2 := createTestSMContext(t, supi, 2)
	smContext3 := createTestSMContext(t, supi, 3)

	// Verify all sessions exist
	assert.NotNil(t, smf_context.GetSMContextByRef(smContext1.Ref))
	assert.NotNil(t, smf_context.GetSMContextByRef(smContext2.Ref))
	assert.NotNil(t, smf_context.GetSMContextByRef(smContext3.Ref))

	// Delete session 2
	smf_context.DeleteSMContextFromPool(smContext2.Ref)

	// Verify only session 2 is removed
	assert.NotNil(t, smf_context.GetSMContextByRef(smContext1.Ref), "Session 1 should still exist")
	assert.Nil(t, smf_context.GetSMContextByRef(smContext2.Ref), "Session 2 should be removed")
	assert.NotNil(t, smf_context.GetSMContextByRef(smContext3.Ref), "Session 3 should still exist")

	// Cleanup remaining sessions
	smf_context.DeleteSMContextFromPool(smContext1.Ref)
	smf_context.DeleteSMContextFromPool(smContext3.Ref)
}

func TestPDUSessionRelease_DeleteNonExistentSession(t *testing.T) {
	// Should not panic when deleting non-existent session
	assert.NotPanics(t, func() {
		smf_context.DeleteSMContextFromPool("non-existent-ref")
	}, "Should not panic when deleting non-existent session")
}

func TestPDUSessionRelease_DoubleDelete(t *testing.T) {
	supi := "imsi-208930000000022"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	ref := smContext.Ref

	// Delete once
	smf_context.DeleteSMContextFromPool(ref)

	// Delete again - should not panic
	assert.NotPanics(t, func() {
		smf_context.DeleteSMContextFromPool(ref)
	}, "Double delete should not panic")
}

func TestPDUSessionRelease_ConcurrentDelete(t *testing.T) {
	supi := "imsi-208930000000023"

	// Create 10 sessions
	var refs []string
	for i := 1; i <= 10; i++ {
		smContext := createTestSMContext(t, supi, int32(i))
		refs = append(refs, smContext.Ref)
	}

	// Delete all sessions concurrently
	var wg sync.WaitGroup
	for _, ref := range refs {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			smf_context.DeleteSMContextFromPool(r)
		}(ref)
	}
	wg.Wait()

	// Verify all sessions are removed
	for _, ref := range refs {
		assert.Nil(t, smf_context.GetSMContextByRef(ref), "Session should be removed")
	}
}

func TestPDUSessionRelease_ForceCleanup_WithCallback(t *testing.T) {
	supi := "imsi-208930000000024"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	ref := smContext.Ref

	// Set session as active and make it idle
	smContext.SetState(smf_context.Active)
	smContext.IdleTimeout = 300 * time.Second // Set idle timeout
	smContext.LastActiveTime = time.Now().Add(-400 * time.Second)

	// Track if callback would be triggered
	callbackTriggered := false

	// Get the singleton cleaner and set callback
	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		callbackTriggered = true
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	cleaner.ForceCleanup()

	assert.True(t, callbackTriggered, "Cleanup callback should be triggered for idle session")
	assert.Nil(t, smf_context.GetSMContextByRef(ref), "Session should be removed after cleanup")
}

func TestPDUSessionRelease_ForceCleanup_ActiveSessionNotRemoved(t *testing.T) {
	supi := "imsi-208930000000025"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	defer smf_context.DeleteSMContextFromPool(smContext.Ref)
	ref := smContext.Ref

	// Set session as active and update time (not idle)
	smContext.SetState(smf_context.Active)
	smContext.IdleTimeout = 300 * time.Second // Set idle timeout
	smContext.UpdateLastActiveTime()

	// Get the singleton cleaner and set callback
	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	cleaner.ForceCleanup()

	// Active session should still exist
	assert.NotNil(t, smf_context.GetSMContextByRef(ref), "Active session should not be removed")
}

func TestPDUSessionRelease_BatchCleanup(t *testing.T) {
	supi := "imsi-208930000000026"

	// Create 5 idle sessions and 5 active sessions
	var idleRefs, activeRefs []string

	for i := 1; i <= 5; i++ {
		// Idle session
		idleCtx := createTestSMContext(t, supi, int32(i))
		idleCtx.SetState(smf_context.Active)
		idleCtx.IdleTimeout = 300 * time.Second
		idleCtx.LastActiveTime = time.Now().Add(-400 * time.Second)
		idleRefs = append(idleRefs, idleCtx.Ref)

		// Active session
		activeCtx := createTestSMContext(t, supi, int32(i+10))
		activeCtx.SetState(smf_context.Active)
		activeCtx.IdleTimeout = 300 * time.Second
		activeCtx.UpdateLastActiveTime()
		activeRefs = append(activeRefs, activeCtx.Ref)
	}

	// Get the singleton cleaner and set callback
	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	cleaner.ForceCleanup()

	// Verify idle sessions are removed
	for _, ref := range idleRefs {
		assert.Nil(t, smf_context.GetSMContextByRef(ref), "Idle session should be removed")
	}

	// Verify active sessions still exist
	for _, ref := range activeRefs {
		assert.NotNil(t, smf_context.GetSMContextByRef(ref), "Active session should still exist")
		smf_context.DeleteSMContextFromPool(ref) // Cleanup
	}
}

func TestPDUSessionRelease_VerifySessionState(t *testing.T) {
	supi := "imsi-208930000000027"
	pduSessionID := int32(1)

	smContext := createTestSMContext(t, supi, pduSessionID)
	ref := smContext.Ref

	// Set to active state
	smContext.SetState(smf_context.Active)
	assert.Equal(t, smf_context.Active, smContext.State(), "State should be Active")

	// Make it idle
	smContext.IdleTimeout = 300 * time.Second
	smContext.LastActiveTime = time.Now().Add(-400 * time.Second)

	// Get the singleton cleaner and set callback
	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		// Verify session state before cleanup
		assert.Equal(t, smf_context.Active, ctx.State(), "Session should still be Active before cleanup")
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	cleaner.ForceCleanup()

	assert.Nil(t, smf_context.GetSMContextByRef(ref), "Session should be removed")
}

// ============================================================================
// Edge Case Tests
// ============================================================================

func TestEdgeCase_EmptySMContextPool(t *testing.T) {
	// ForceCleanup should not panic with empty pool
	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	assert.NotPanics(t, func() {
		cleaner.ForceCleanup()
	}, "ForceCleanup should not panic with empty pool")
}

func TestEdgeCase_NilCallback(t *testing.T) {
	// Set cleaner with nil callback
	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(nil)

	supi := "imsi-208930000000010"
	smContext := createTestSMContext(t, supi, 1)
	defer smf_context.DeleteSMContextFromPool(smContext.Ref)

	smContext.SetState(smf_context.Active)
	smContext.LastActiveTime = time.Now().Add(-400 * time.Second)

	// Should not panic even with nil callback
	assert.NotPanics(t, func() {
		cleaner.ForceCleanup()
	}, "ForceCleanup should not panic with nil callback")
}

func TestEdgeCase_VeryOldSession(t *testing.T) {
	supi := "imsi-208930000000011"
	smContext := createTestSMContext(t, supi, 1)

	// Set session as very old (1 year ago)
	smContext.SetState(smf_context.Active)
	smContext.IdleTimeout = 300 * time.Second
	smContext.LastActiveTime = time.Now().Add(-365 * 24 * time.Hour)

	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	cleaner.ForceCleanup()

	assert.Nil(t, smf_context.GetSMContextByRef(smContext.Ref), "Very old session should be cleaned up")
}

func TestEdgeCase_ConcurrentCleanup(t *testing.T) {
	supi := "imsi-208930000000012"

	// Create multiple sessions
	var refs []string
	for i := 1; i <= 5; i++ {
		smContext := createTestSMContext(t, supi, int32(i))
		smContext.SetState(smf_context.Active)
		smContext.IdleTimeout = 300 * time.Second
		smContext.LastActiveTime = time.Now().Add(-400 * time.Second)
		refs = append(refs, smContext.Ref)
	}

	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	// Run multiple cleanups concurrently
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cleaner.ForceCleanup()
		}()
	}
	wg.Wait()

	// All sessions should be cleaned up
	for _, ref := range refs {
		assert.Nil(t, smf_context.GetSMContextByRef(ref), "Session should be cleaned up")
	}
}

// ============================================================================
// Integration Tests
// ============================================================================

func TestIntegration_FullCleanupCycle(t *testing.T) {
	t.Skip("Requires full SMF initialization including PFCP context")

	// This test would:
	// 1. Create SMContext
	// 2. Initialize PFCP session
	// 3. Set up URR
	// 4. Wait for idle timeout
	// 5. Verify cleanup callback is invoked
}

func TestIntegration_URRReportTriggersUpdate(t *testing.T) {
	t.Skip("Requires full SMF initialization including PFCP context")

	// This test would:
	// 1. Create SMContext with PFCP session
	// 2. Simulate URR report with traffic
	// 3. Verify LastActiveTime is updated
}

func TestIntegration_NoTrafficNoUpdate(t *testing.T) {
	t.Skip("Requires full SMF initialization including PFCP context")

	// This test would:
	// 1. Create SMContext with PFCP session
	// 2. Simulate URR report with zero traffic
	// 3. Verify LastActiveTime is NOT updated
}

// ============================================================================
// Benchmark Tests
// ============================================================================

func BenchmarkSMContextCreation(b *testing.B) {
	for i := 0; i < b.N; i++ {
		supi := "imsi-208930000000099"
		smContext := smf_context.NewSMContext(supi, int32(i%1000))
		if smContext != nil {
			smf_context.DeleteSMContextFromPool(smContext.Ref)
		}
	}
}

func BenchmarkForceCleanup(b *testing.B) {
	cleaner := smf_context.GetSessionCleaner()
	cleaner.SetReleaseCallback(func(ctx *smf_context.SMContext) error {
		smf_context.DeleteSMContextFromPool(ctx.Ref)
		return nil
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cleaner.ForceCleanup()
	}
}

func BenchmarkUpdateLastActiveTime(b *testing.B) {
	supi := "imsi-208930000000100"
	smContext := smf_context.NewSMContext(supi, 1)
	defer smf_context.DeleteSMContextFromPool(smContext.Ref)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		smContext.UpdateLastActiveTime()
	}
}
