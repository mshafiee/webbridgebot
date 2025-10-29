package web

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ConnectionInfo tracks information about an active streaming connection
type ConnectionInfo struct {
	MessageID      int
	ClientAddr     string
	StartTime      time.Time
	BytesStreamed  int64
	RangeStart     int64
	RangeEnd       int64
	LastActivity   time.Time
	ConnectionID   string
	Completed      bool
	DisconnectTime *time.Time
	Error          error
}

// ConnectionTracker manages and monitors active streaming connections
type ConnectionTracker struct {
	mu          sync.RWMutex
	connections map[string]*ConnectionInfo

	// Statistics
	totalConnections    int64
	completedStreams    int64
	disconnectedStreams int64
	erroredStreams      int64
	totalBytesStreamed  int64

	// Connection limits and timeouts
	maxIdleTime     time.Duration
	cleanupInterval time.Duration
	ctx             context.Context
	cancel          context.CancelFunc
}

// NewConnectionTracker creates a new connection tracker
func NewConnectionTracker(maxIdleTime time.Duration, cleanupInterval time.Duration) *ConnectionTracker {
	ctx, cancel := context.WithCancel(context.Background())

	tracker := &ConnectionTracker{
		connections:     make(map[string]*ConnectionInfo),
		maxIdleTime:     maxIdleTime,
		cleanupInterval: cleanupInterval,
		ctx:             ctx,
		cancel:          cancel,
	}

	// Start background cleanup routine
	go tracker.cleanupRoutine()

	return tracker
}

// RegisterConnection registers a new streaming connection
func (ct *ConnectionTracker) RegisterConnection(messageID int, clientAddr string, rangeStart, rangeEnd int64) string {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	connectionID := fmt.Sprintf("%s-%d-%d", clientAddr, messageID, time.Now().Unix())
	now := time.Now()

	ct.connections[connectionID] = &ConnectionInfo{
		MessageID:     messageID,
		ClientAddr:    clientAddr,
		StartTime:     now,
		LastActivity:  now,
		RangeStart:    rangeStart,
		RangeEnd:      rangeEnd,
		ConnectionID:  connectionID,
		BytesStreamed: 0,
		Completed:     false,
	}

	ct.totalConnections++

	return connectionID
}

// UpdateActivity updates the last activity time for a connection
func (ct *ConnectionTracker) UpdateActivity(connectionID string, bytesStreamed int64) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if conn, exists := ct.connections[connectionID]; exists {
		conn.LastActivity = time.Now()
		conn.BytesStreamed = bytesStreamed
	}
}

// MarkCompleted marks a connection as successfully completed
func (ct *ConnectionTracker) MarkCompleted(connectionID string, bytesStreamed int64) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if conn, exists := ct.connections[connectionID]; exists {
		conn.Completed = true
		conn.BytesStreamed = bytesStreamed
		now := time.Now()
		conn.DisconnectTime = &now
		ct.completedStreams++
		ct.totalBytesStreamed += bytesStreamed
	}
}

// MarkDisconnected marks a connection as disconnected
func (ct *ConnectionTracker) MarkDisconnected(connectionID string, bytesStreamed int64) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if conn, exists := ct.connections[connectionID]; exists {
		now := time.Now()
		conn.DisconnectTime = &now
		conn.BytesStreamed = bytesStreamed
		ct.disconnectedStreams++
		ct.totalBytesStreamed += bytesStreamed
	}
}

// MarkError marks a connection as errored
func (ct *ConnectionTracker) MarkError(connectionID string, bytesStreamed int64, err error) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if conn, exists := ct.connections[connectionID]; exists {
		now := time.Now()
		conn.DisconnectTime = &now
		conn.BytesStreamed = bytesStreamed
		conn.Error = err
		ct.erroredStreams++
		ct.totalBytesStreamed += bytesStreamed
	}
}

// GetActiveConnections returns the number of currently active connections
func (ct *ConnectionTracker) GetActiveConnections() int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	active := 0
	now := time.Now()

	for _, conn := range ct.connections {
		if conn.DisconnectTime == nil && now.Sub(conn.LastActivity) < ct.maxIdleTime {
			active++
		}
	}

	return active
}

// GetConnectionsByMessageID returns all connections (including recent historical) for a message ID
func (ct *ConnectionTracker) GetConnectionsByMessageID(messageID int) []*ConnectionInfo {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	var connections []*ConnectionInfo
	for _, conn := range ct.connections {
		if conn.MessageID == messageID {
			// Create a copy to avoid race conditions
			connCopy := *conn
			connections = append(connections, &connCopy)
		}
	}

	return connections
}

// GetStatistics returns connection statistics
func (ct *ConnectionTracker) GetStatistics() map[string]interface{} {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	return map[string]interface{}{
		"total_connections":    ct.totalConnections,
		"active_connections":   ct.GetActiveConnections(),
		"completed_streams":    ct.completedStreams,
		"disconnected_streams": ct.disconnectedStreams,
		"errored_streams":      ct.erroredStreams,
		"total_bytes_streamed": ct.totalBytesStreamed,
		"total_mb_streamed":    float64(ct.totalBytesStreamed) / (1024 * 1024),
		"total_gb_streamed":    float64(ct.totalBytesStreamed) / (1024 * 1024 * 1024),
	}
}

// cleanupRoutine periodically removes old connection records
func (ct *ConnectionTracker) cleanupRoutine() {
	ticker := time.NewTicker(ct.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ct.ctx.Done():
			return
		case <-ticker.C:
			ct.cleanup()
		}
	}
}

// cleanup removes old connection records
func (ct *ConnectionTracker) cleanup() {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-10 * time.Minute) // Keep records for 10 minutes

	for id, conn := range ct.connections {
		// Remove if disconnected/completed more than 10 minutes ago
		if conn.DisconnectTime != nil && conn.DisconnectTime.Before(cutoff) {
			delete(ct.connections, id)
		}
		// Also remove if no activity for more than 10 minutes
		if conn.DisconnectTime == nil && conn.LastActivity.Before(cutoff) {
			delete(ct.connections, id)
		}
	}
}

// Stop stops the connection tracker and cleanup routine
func (ct *ConnectionTracker) Stop() {
	ct.cancel()
}

// DetectReconnection detects if this is a reconnection attempt for a recent connection
func (ct *ConnectionTracker) DetectReconnection(messageID int, clientAddr string, rangeStart int64) (bool, *ConnectionInfo) {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	now := time.Now()
	reconnectionWindow := 30 * time.Second // Consider it a reconnection if within 30 seconds

	for _, conn := range ct.connections {
		if conn.MessageID == messageID &&
			conn.ClientAddr == clientAddr &&
			conn.DisconnectTime != nil &&
			now.Sub(*conn.DisconnectTime) < reconnectionWindow {
			// This appears to be a reconnection
			connCopy := *conn
			return true, &connCopy
		}
	}

	return false, nil
}

