package web

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/logger"

	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsClientDisconnectError tests the client disconnection detection
func TestIsClientDisconnectError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "EPIPE error",
			err:      syscall.EPIPE,
			expected: true,
		},
		{
			name:     "ECONNRESET error",
			err:      syscall.ECONNRESET,
			expected: true,
		},
		{
			name:     "ECONNABORTED error",
			err:      syscall.ECONNABORTED,
			expected: true,
		},
		{
			name:     "broken pipe string",
			err:      errors.New("write tcp: broken pipe"),
			expected: true,
		},
		{
			name:     "connection reset by peer",
			err:      errors.New("read tcp: connection reset by peer"),
			expected: true,
		},
		{
			name:     "connection reset",
			err:      errors.New("connection reset"),
			expected: true,
		},
		{
			name:     "client disconnected",
			err:      errors.New("client disconnected"),
			expected: true,
		},
		{
			name:     "readfrom tcp error",
			err:      errors.New("readfrom tcp 192.168.1.1:8080: connection reset"),
			expected: true,
		},
		{
			name:     "unrelated error",
			err:      errors.New("some other error"),
			expected: false,
		},
		{
			name:     "EOF error",
			err:      io.EOF,
			expected: false,
		},
		{
			name:     "context cancelled",
			err:      context.Canceled,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isClientDisconnectError(tt.err)
			assert.Equal(t, tt.expected, result, "isClientDisconnectError(%v) = %v, expected %v", tt.err, result, tt.expected)
		})
	}
}

// TestParseChatID tests the chat ID parsing helper
func TestParseChatID(t *testing.T) {
	tests := []struct {
		name      string
		vars      map[string]string
		expected  int64
		expectErr bool
	}{
		{
			name:      "valid positive chat ID",
			vars:      map[string]string{"chatID": "123456789"},
			expected:  123456789,
			expectErr: false,
		},
		{
			name:      "valid negative chat ID",
			vars:      map[string]string{"chatID": "-1001234567890"},
			expected:  -1001234567890,
			expectErr: false,
		},
		{
			name:      "missing chat ID",
			vars:      map[string]string{},
			expected:  0,
			expectErr: true,
		},
		{
			name:      "invalid chat ID format",
			vars:      map[string]string{"chatID": "invalid"},
			expected:  0,
			expectErr: true,
		},
		{
			name:      "empty chat ID",
			vars:      map[string]string{"chatID": ""},
			expected:  0,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseChatID(tt.vars)

			if tt.expectErr {
				assert.Error(t, err, "Expected error for input: %v", tt.vars)
			} else {
				assert.NoError(t, err, "Expected no error for input: %v", tt.vars)
				assert.Equal(t, tt.expected, result, "Expected chatID %d, got %d", tt.expected, result)
			}
		})
	}
}

// TestHandleConnectionStats tests the connection statistics endpoint
func TestHandleConnectionStats(t *testing.T) {
	// Setup
	log := logger.New(io.Discard, "test", logger.INFO, false)
	cfg := &config.Configuration{
		DebugMode: false,
	}

	// Create in-memory SQLite database for testing
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err, "Failed to create test database")
	defer db.Close()

	// Create user repository
	userRepo := data.NewUserRepository(db)
	err = userRepo.InitDB()
	require.NoError(t, err, "Failed to initialize test database")

	// Create server with connection tracker
	server := &Server{
		config:         cfg,
		logger:         log,
		userRepository: userRepo,
		connTracker:    NewConnectionTracker(5*time.Minute, 1*time.Minute),
	}
	defer server.connTracker.Stop()

	// Add a test user to the repository
	testChatID := int64(123456789)
	err = userRepo.StoreUserInfo(testChatID, testChatID, "TestUser", "", "", true, false)
	require.NoError(t, err, "Failed to save test user")

	// Register some test connections
	server.connTracker.RegisterConnection(1234, "192.168.1.100:50000", 0, 1048576)
	conn2 := server.connTracker.RegisterConnection(1234, "192.168.1.100:50001", 1048576, 2097152)
	server.connTracker.MarkCompleted(conn2, 1048576)

	t.Run("unauthorized request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/connection-stats/999999999", nil)
		req = mux.SetURLVars(req, map[string]string{"chatID": "999999999"})
		rr := httptest.NewRecorder()

		server.handleConnectionStats(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "Should return 401 for unauthorized user")
	})

	t.Run("invalid chat ID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/connection-stats/invalid", nil)
		req = mux.SetURLVars(req, map[string]string{"chatID": "invalid"})
		rr := httptest.NewRecorder()

		server.handleConnectionStats(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code, "Should return 400 for invalid chat ID")
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/connection-stats/123456789", nil)
		req = mux.SetURLVars(req, map[string]string{"chatID": "123456789"})
		rr := httptest.NewRecorder()

		server.handleConnectionStats(rr, req)

		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, "Should return 405 for non-GET methods")
	})

	t.Run("successful stats request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/connection-stats/123456789", nil)
		req = mux.SetURLVars(req, map[string]string{"chatID": "123456789"})
		rr := httptest.NewRecorder()

		server.handleConnectionStats(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "Should return 200 for authorized user")
		assert.Equal(t, "application/json", rr.Header().Get("Content-Type"), "Should return JSON")
		assert.Contains(t, rr.Body.String(), "total_connections", "Response should contain statistics")
	})

	t.Run("stats with message ID filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/connection-stats/123456789?messageId=1234", nil)
		req = mux.SetURLVars(req, map[string]string{"chatID": "123456789"})
		rr := httptest.NewRecorder()

		server.handleConnectionStats(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "Should return 200 for authorized user")
		assert.Contains(t, rr.Body.String(), "message_connections", "Response should contain message-specific connections")
	})
}

// TestConnectionTrackerIntegration tests the integration of connection tracker with handlers
func TestConnectionTrackerIntegration(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Simulate a stream request lifecycle
	messageID := 1234
	clientAddr := "192.168.1.100:50000"
	rangeStart := int64(0)
	rangeEnd := int64(1048576) // 1 MB

	// Register connection
	connID := tracker.RegisterConnection(messageID, clientAddr, rangeStart, rangeEnd)
	assert.NotEmpty(t, connID, "Connection ID should be generated")

	// Simulate streaming progress
	tracker.UpdateActivity(connID, 262144) // 256 KB
	time.Sleep(10 * time.Millisecond)
	tracker.UpdateActivity(connID, 524288) // 512 KB
	time.Sleep(10 * time.Millisecond)
	tracker.UpdateActivity(connID, 786432) // 768 KB

	// Simulate client disconnection
	tracker.MarkDisconnected(connID, 786432)

	stats := tracker.GetStatistics()
	assert.Equal(t, int64(1), stats["disconnected_streams"], "Should track disconnection")

	// Simulate reconnection
	time.Sleep(50 * time.Millisecond)
	isReconnection, prevConn := tracker.DetectReconnection(messageID, clientAddr, rangeEnd)

	assert.True(t, isReconnection, "Should detect reconnection")
	assert.NotNil(t, prevConn, "Should provide previous connection info")
	assert.Equal(t, int64(786432), prevConn.BytesStreamed, "Should track previous bytes")

	// Register new connection (reconnection)
	connID2 := tracker.RegisterConnection(messageID, clientAddr, 786432, rangeEnd)

	// Complete the stream
	tracker.MarkCompleted(connID2, rangeEnd-786432)

	stats = tracker.GetStatistics()
	assert.Equal(t, int64(2), stats["total_connections"], "Should track both connections")
	assert.Equal(t, int64(1), stats["completed_streams"], "Should track completion")
	assert.Equal(t, int64(1), stats["disconnected_streams"], "Should track disconnection")
}

// TestConnectionTrackerCleanupIntegration tests cleanup in realistic scenarios
func TestConnectionTrackerCleanupIntegration(t *testing.T) {
	// Use short intervals for testing
	tracker := NewConnectionTracker(100*time.Millisecond, 50*time.Millisecond)
	defer tracker.Stop()

	// Create several connections
	for i := 0; i < 5; i++ {
		connID := tracker.RegisterConnection(i, "192.168.1.100:50000", 0, 1024)
		tracker.MarkCompleted(connID, 1024)
	}

	stats := tracker.GetStatistics()
	assert.Equal(t, int64(5), stats["total_connections"], "Should have 5 connections")

	// Manually set old disconnect times to trigger cleanup
	tracker.mu.Lock()
	for _, conn := range tracker.connections {
		if conn.DisconnectTime != nil {
			oldTime := time.Now().Add(-15 * time.Minute)
			conn.DisconnectTime = &oldTime
		}
	}
	tracker.mu.Unlock()

	// Trigger cleanup
	tracker.cleanup()

	// Verify connections were cleaned up
	tracker.mu.RLock()
	remainingCount := len(tracker.connections)
	tracker.mu.RUnlock()

	assert.Equal(t, 0, remainingCount, "Old connections should be cleaned up")

	// Stats should persist
	stats = tracker.GetStatistics()
	assert.Equal(t, int64(5), stats["total_connections"], "Total connections stat should persist")
}

// TestReconnectionPatternDetection tests realistic reconnection patterns
func TestReconnectionPatternDetection(t *testing.T) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	messageID := 3519
	clientAddr := "95.90.241.89:39757"
	fileSize := int64(576101836)

	// Simulate the pattern from the user's log file
	connections := []struct {
		start int64
		end   int64
		bytes int64
		delay time.Duration
	}{
		{382304256, fileSize - 1, 96898560, 230 * time.Second}, // First connection
		{459177984, fileSize - 1, 58261504, 4 * time.Second},   // Reconnection 1
		{387547136, fileSize - 1, 94330880, 2 * time.Second},   // Reconnection 2
		{390922240, fileSize - 1, 92339200, 0},                 // Reconnection 3
	}

	connIDs := make([]string, 0, len(connections))
	for i, conn := range connections {
		if i > 0 {
			// Wait for the delay
			time.Sleep(10 * time.Millisecond) // Shortened for testing

			// Check for reconnection
			isReconn, _ := tracker.DetectReconnection(messageID, clientAddr, conn.start)
			if conn.delay < 30*time.Second {
				assert.True(t, isReconn, "Should detect reconnection for connection %d", i)
			}
		}

		// Register and stream
		connID := tracker.RegisterConnection(messageID, clientAddr, conn.start, conn.end)
		connIDs = append(connIDs, connID)
		tracker.UpdateActivity(connID, conn.bytes/2) // Simulate partial streaming
		tracker.MarkDisconnected(connID, conn.bytes)
	}

	// Verify statistics
	stats := tracker.GetStatistics()
	assert.Equal(t, int64(4), stats["total_connections"], "Should have 4 connection attempts")
	assert.Equal(t, int64(4), stats["disconnected_streams"], "All should be disconnected")

	// Verify message-specific connections (should get all connections we just created)
	conns := tracker.GetConnectionsByMessageID(messageID)
	assert.GreaterOrEqual(t, len(conns), 1, "Should retrieve at least one connection for message")
	assert.LessOrEqual(t, len(conns), 4, "Should not retrieve more than 4 connections")

	// Verify all our connection IDs are present in the tracker
	for _, connID := range connIDs {
		tracker.mu.RLock()
		_, exists := tracker.connections[connID]
		tracker.mu.RUnlock()
		assert.True(t, exists, "Connection %s should exist in tracker", connID)
	}
}

// BenchmarkConnectionTracker_Register benchmarks connection registration
func BenchmarkConnectionTracker_Register(b *testing.B) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracker.RegisterConnection(i, "192.168.1.100:50000", 0, 1024)
	}
}

// BenchmarkConnectionTracker_Statistics benchmarks statistics retrieval
func BenchmarkConnectionTracker_Statistics(b *testing.B) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Populate with some connections
	for i := 0; i < 100; i++ {
		connID := tracker.RegisterConnection(i, "192.168.1.100:50000", 0, 1024)
		tracker.MarkCompleted(connID, 1024)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracker.GetStatistics()
	}
}

// BenchmarkConnectionTracker_DetectReconnection benchmarks reconnection detection
func BenchmarkConnectionTracker_DetectReconnection(b *testing.B) {
	tracker := NewConnectionTracker(5*time.Minute, 1*time.Minute)
	defer tracker.Stop()

	// Create some disconnected connections
	for i := 0; i < 50; i++ {
		connID := tracker.RegisterConnection(i, "192.168.1.100:50000", 0, 1024)
		tracker.MarkDisconnected(connID, 512)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracker.DetectReconnection(25, "192.168.1.100:50000", 512)
	}
}
