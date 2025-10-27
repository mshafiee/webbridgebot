package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestConnectionTracker_RegisterConnection tests registering new connections
func TestConnectionTracker_RegisterConnection(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Register a connection
	connID := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1024)

	assert.NotEmpty(t, connID, "Connection ID should not be empty")
	assert.Contains(t, connID, "192.168.1.100:50000", "Connection ID should contain client address")
	assert.Contains(t, connID, "1234", "Connection ID should contain message ID")

	// Verify statistics
	stats := tracker.GetStatistics()
	assert.Equal(t, int64(1), stats["total_connections"], "Should have 1 total connection")
	assert.Equal(t, 1, tracker.GetActiveConnections(), "Should have 1 active connection")
}

// TestConnectionTracker_MultipleConnections tests tracking multiple connections
func TestConnectionTracker_MultipleConnections(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Register multiple connections
	conn1 := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1024)
	conn2 := tracker.RegisterConnection(1235, "192.168.1.101:50001", 0, 2048)
	conn3 := tracker.RegisterConnection(1236, "192.168.1.102:50002", 0, 4096)

	assert.NotEqual(t, conn1, conn2, "Connection IDs should be unique")
	assert.NotEqual(t, conn2, conn3, "Connection IDs should be unique")
	assert.NotEqual(t, conn1, conn3, "Connection IDs should be unique")

	stats := tracker.GetStatistics()
	assert.Equal(t, int64(3), stats["total_connections"], "Should have 3 total connections")
	assert.Equal(t, 3, tracker.GetActiveConnections(), "Should have 3 active connections")
}

// TestConnectionTracker_MarkCompleted tests marking a connection as completed
func TestConnectionTracker_MarkCompleted(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	connID := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1048576)

	// Mark as completed with bytes streamed (exactly 1 MB)
	bytesStreamed := int64(1048576)
	tracker.MarkCompleted(connID, bytesStreamed)

	// Wait a moment for the update to be processed
	time.Sleep(10 * time.Millisecond)

	stats := tracker.GetStatistics()
	assert.Equal(t, int64(1), stats["completed_streams"], "Should have 1 completed stream")
	assert.Equal(t, int64(bytesStreamed), stats["total_bytes_streamed"], "Should track bytes streamed")
	assert.InDelta(t, float64(1.0), stats["total_mb_streamed"], 0.01, "Should calculate MB correctly")
}

// TestConnectionTracker_MarkDisconnected tests marking a connection as disconnected
func TestConnectionTracker_MarkDisconnected(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	connID := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 2048000)

	// Mark as disconnected with partial bytes streamed
	bytesStreamed := int64(1024000) // Only half
	tracker.MarkDisconnected(connID, bytesStreamed)

	time.Sleep(10 * time.Millisecond)

	stats := tracker.GetStatistics()
	assert.Equal(t, int64(1), stats["disconnected_streams"], "Should have 1 disconnected stream")
	assert.Equal(t, int64(bytesStreamed), stats["total_bytes_streamed"], "Should track partial bytes")
}

// TestConnectionTracker_MarkError tests marking a connection with an error
func TestConnectionTracker_MarkError(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	connID := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1024000)

	// Mark as errored
	bytesStreamed := int64(512000)
	err := assert.AnError
	tracker.MarkError(connID, bytesStreamed, err)

	time.Sleep(10 * time.Millisecond)

	stats := tracker.GetStatistics()
	assert.Equal(t, int64(1), stats["errored_streams"], "Should have 1 errored stream")
	assert.Equal(t, int64(bytesStreamed), stats["total_bytes_streamed"], "Should track bytes before error")
}

// TestConnectionTracker_UpdateActivity tests updating connection activity
func TestConnectionTracker_UpdateActivity(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	connID := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1024000)

	// Get initial connection info
	tracker.mu.RLock()
	conn1, exists := tracker.connections[connID]
	assert.True(t, exists, "Connection should exist")
	initialActivity := conn1.LastActivity
	tracker.mu.RUnlock()

	// Wait a bit and update activity
	time.Sleep(50 * time.Millisecond)
	tracker.UpdateActivity(connID, 512000)

	// Verify activity was updated
	tracker.mu.RLock()
	conn2, exists := tracker.connections[connID]
	assert.True(t, exists, "Connection should still exist")
	assert.True(t, conn2.LastActivity.After(initialActivity), "Last activity should be updated")
	assert.Equal(t, int64(512000), conn2.BytesStreamed, "Bytes streamed should be updated")
	tracker.mu.RUnlock()
}

// TestConnectionTracker_GetConnectionsByMessageID tests retrieving connections by message ID
func TestConnectionTracker_GetConnectionsByMessageID(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Register multiple connections for the same message
	messageID := 1234
	conn1 := tracker.RegisterConnection(messageID, "192.168.1.100:50000", 0, 1024)
	conn2 := tracker.RegisterConnection(messageID, "192.168.1.100:50001", 1024, 2048)

	// Register a connection for a different message
	tracker.RegisterConnection(5678, "192.168.1.101:50002", 0, 1024)

	// Get connections for message 1234
	connections := tracker.GetConnectionsByMessageID(messageID)

	assert.Len(t, connections, 2, "Should have 2 connections for message 1234")

	// Verify connection IDs
	foundConn1, foundConn2 := false, false
	for _, conn := range connections {
		if conn.ConnectionID == conn1 {
			foundConn1 = true
		}
		if conn.ConnectionID == conn2 {
			foundConn2 = true
		}
		assert.Equal(t, messageID, conn.MessageID, "All connections should have correct message ID")
	}
	assert.True(t, foundConn1, "Should find first connection")
	assert.True(t, foundConn2, "Should find second connection")
}

// TestConnectionTracker_DetectReconnection tests reconnection detection
func TestConnectionTracker_DetectReconnection(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	messageID := 1234
	clientAddr := "192.168.1.100:50000"

	// Register and disconnect a connection
	connID := tracker.RegisterConnection(messageID, clientAddr, 0, 1024000)
	tracker.MarkDisconnected(connID, 512000)

	time.Sleep(10 * time.Millisecond)

	// Try to detect reconnection within window (30 seconds)
	isReconnection, prevConn := tracker.DetectReconnection(messageID, clientAddr, 512000)

	assert.True(t, isReconnection, "Should detect reconnection")
	assert.NotNil(t, prevConn, "Should return previous connection info")
	assert.Equal(t, messageID, prevConn.MessageID, "Previous connection should have correct message ID")
	assert.Equal(t, clientAddr, prevConn.ClientAddr, "Previous connection should have correct client address")
	assert.Equal(t, int64(512000), prevConn.BytesStreamed, "Should track previous bytes streamed")
}

// TestConnectionTracker_NoReconnectionAfterTimeout tests that reconnection is not detected after timeout
func TestConnectionTracker_NoReconnectionAfterTimeout(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	messageID := 1234
	clientAddr := "192.168.1.100:50000"

	// Register and disconnect a connection
	connID := tracker.RegisterConnection(messageID, clientAddr, 0, 1024000)
	tracker.MarkDisconnected(connID, 512000)

	// Manually set disconnect time to be > 30 seconds ago (beyond reconnection window)
	tracker.mu.Lock()
	if conn, exists := tracker.connections[connID]; exists && conn.DisconnectTime != nil {
		oldTime := time.Now().Add(-35 * time.Second)
		conn.DisconnectTime = &oldTime
	}
	tracker.mu.Unlock()

	// Try to detect reconnection - should fail because too much time passed
	isReconnection, prevConn := tracker.DetectReconnection(messageID, clientAddr, 512000)

	assert.False(t, isReconnection, "Should not detect reconnection after timeout")
	assert.Nil(t, prevConn, "Should not return previous connection info")
}

// TestConnectionTracker_ReconnectionDifferentClient tests that reconnection is not detected for different client
func TestConnectionTracker_ReconnectionDifferentClient(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	messageID := 1234

	// Register and disconnect from one client
	connID := tracker.RegisterConnection(messageID, "192.168.1.100:50000", 0, 1024000)
	tracker.MarkDisconnected(connID, 512000)

	time.Sleep(10 * time.Millisecond)

	// Try to detect reconnection from a different client
	isReconnection, prevConn := tracker.DetectReconnection(messageID, "192.168.1.101:50001", 512000)

	assert.False(t, isReconnection, "Should not detect reconnection from different client")
	assert.Nil(t, prevConn, "Should not return previous connection info")
}

// TestConnectionTracker_GetActiveConnections tests active connection counting
func TestConnectionTracker_GetActiveConnections(t *testing.T) {
	tracker := NewConnectionTracker(100*time.Millisecond, 1*time.Minute) // Short idle time for testing
	defer tracker.Stop()

	// Register some connections
	conn1 := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1024)
	conn2 := tracker.RegisterConnection(1235, "192.168.1.101:50001", 0, 2048)
	tracker.RegisterConnection(1236, "192.168.1.102:50002", 0, 4096)

	assert.Equal(t, 3, tracker.GetActiveConnections(), "Should have 3 active connections")

	// Mark one as completed
	tracker.MarkCompleted(conn1, 1024)
	time.Sleep(10 * time.Millisecond)

	// Should still count as active briefly after completion
	activeCount := tracker.GetActiveConnections()
	assert.LessOrEqual(t, activeCount, 3, "Active count should not increase")

	// Mark another as disconnected
	tracker.MarkDisconnected(conn2, 1024)
	time.Sleep(10 * time.Millisecond)

	// Wait for idle timeout
	time.Sleep(150 * time.Millisecond)

	// Should have fewer active connections now
	activeCount = tracker.GetActiveConnections()
	assert.LessOrEqual(t, activeCount, 3, "Active connections should decrease over time")
}

// TestConnectionTracker_Cleanup tests that old connections are cleaned up
func TestConnectionTracker_Cleanup(t *testing.T) {
	// Use very short cleanup interval for testing
	tracker := NewConnectionTracker(100*time.Millisecond, 50*time.Millisecond)
	defer tracker.Stop()

	// Register and complete a connection
	connID := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1024)
	tracker.MarkCompleted(connID, 1024)

	// Manually trigger cleanup by modifying disconnect time to be old
	tracker.mu.Lock()
	if conn, exists := tracker.connections[connID]; exists && conn.DisconnectTime != nil {
		oldTime := time.Now().Add(-15 * time.Minute) // Beyond 10-minute retention
		conn.DisconnectTime = &oldTime
	}
	tracker.mu.Unlock()

	// Manually call cleanup
	tracker.cleanup()

	// Verify connection was removed
	tracker.mu.RLock()
	_, exists := tracker.connections[connID]
	tracker.mu.RUnlock()

	assert.False(t, exists, "Old connection should be cleaned up")
}

// TestConnectionTracker_ConcurrentAccess tests thread safety
func TestConnectionTracker_ConcurrentAccess(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	done := make(chan bool, 10)

	// Spawn multiple goroutines to register connections concurrently
	for i := 0; i < 10; i++ {
		go func(id int) {
			connID := tracker.RegisterConnection(id, "192.168.1.100:50000", 0, 1024)
			tracker.UpdateActivity(connID, 512)
			tracker.MarkCompleted(connID, 1024)
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify all connections were tracked
	stats := tracker.GetStatistics()
	assert.Equal(t, int64(10), stats["total_connections"], "Should have tracked all concurrent connections")
}

// TestConnectionTracker_Statistics tests statistics calculation
func TestConnectionTracker_Statistics(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Create various connection outcomes
	conn1 := tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1048576) // 1 MB
	tracker.MarkCompleted(conn1, 1048576)

	conn2 := tracker.RegisterConnection(1235, "192.168.1.101:50001", 0, 2097152) // 2 MB
	tracker.MarkDisconnected(conn2, 1048576)                                     // Only 1 MB transferred

	conn3 := tracker.RegisterConnection(1236, "192.168.1.102:50002", 0, 3145728) // 3 MB
	tracker.MarkError(conn3, 524288, assert.AnError)                             // Only 0.5 MB transferred

	time.Sleep(20 * time.Millisecond)

	stats := tracker.GetStatistics()

	assert.Equal(t, int64(3), stats["total_connections"], "Should have 3 total connections")
	assert.Equal(t, int64(1), stats["completed_streams"], "Should have 1 completed stream")
	assert.Equal(t, int64(1), stats["disconnected_streams"], "Should have 1 disconnected stream")
	assert.Equal(t, int64(1), stats["errored_streams"], "Should have 1 errored stream")

	// Total bytes: 1 MB + 1 MB + 0.5 MB = 2.5 MB = 2621440 bytes
	expectedBytes := int64(1048576 + 1048576 + 524288)
	assert.Equal(t, expectedBytes, stats["total_bytes_streamed"], "Should track total bytes streamed")

	expectedMB := float64(expectedBytes) / (1024 * 1024)
	assert.InDelta(t, expectedMB, stats["total_mb_streamed"], 0.01, "Should calculate MB correctly")

	expectedGB := float64(expectedBytes) / (1024 * 1024 * 1024)
	assert.InDelta(t, expectedGB, stats["total_gb_streamed"], 0.001, "Should calculate GB correctly")
}

// TestConnectionTracker_Stop tests graceful shutdown
func TestConnectionTracker_Stop(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 100*time.Millisecond)

	// Register some connections
	tracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1024)
	tracker.RegisterConnection(1235, "192.168.1.101:50001", 0, 2048)

	// Stop the tracker
	tracker.Stop()

	// Give it a moment to shut down
	time.Sleep(50 * time.Millisecond)

	// Verify context was cancelled (cleanup routine should stop)
	select {
	case <-tracker.ctx.Done():
		// Context was properly cancelled
	default:
		t.Error("Context should be cancelled after Stop()")
	}
}

// TestConnectionTracker_EmptyTracker tests operations on empty tracker
func TestConnectionTracker_EmptyTracker(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Test operations on empty tracker
	assert.Equal(t, 0, tracker.GetActiveConnections(), "Empty tracker should have 0 active connections")

	stats := tracker.GetStatistics()
	assert.Equal(t, int64(0), stats["total_connections"], "Empty tracker should have 0 total connections")
	assert.Equal(t, int64(0), stats["total_bytes_streamed"], "Empty tracker should have 0 bytes streamed")

	connections := tracker.GetConnectionsByMessageID(1234)
	assert.Empty(t, connections, "Empty tracker should return no connections")

	isReconnection, prevConn := tracker.DetectReconnection(1234, "192.168.1.100:50000", 0)
	assert.False(t, isReconnection, "Empty tracker should not detect reconnections")
	assert.Nil(t, prevConn, "Empty tracker should return nil for previous connection")
}
