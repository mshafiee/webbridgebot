package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// WebSocketManager manages WebSocket connections for different chat IDs
type WebSocketManager struct {
	clients map[int64]*websocket.Conn
}

// NewWebSocketManager creates a new WebSocket manager
func NewWebSocketManager() *WebSocketManager {
	return &WebSocketManager{
		clients: make(map[int64]*websocket.Conn),
	}
}

// AddClient adds a WebSocket connection for a chat ID
func (wm *WebSocketManager) AddClient(chatID int64, conn *websocket.Conn) {
	wm.clients[chatID] = conn
}

// RemoveClient removes a WebSocket connection for a chat ID
func (wm *WebSocketManager) RemoveClient(chatID int64) {
	delete(wm.clients, chatID)
}

// GetClient returns the WebSocket connection for a chat ID
func (wm *WebSocketManager) GetClient(chatID int64) (*websocket.Conn, bool) {
	conn, ok := wm.clients[chatID]
	return conn, ok
}

// PublishMessage sends a message to the WebSocket client for a chat ID
func (wm *WebSocketManager) PublishMessage(chatID int64, message map[string]string) {
	if client, ok := wm.clients[chatID]; ok {
		messageJSON, err := json.Marshal(message)
		if err != nil {
			log.Println("Error marshalling message:", err)
			return
		}
		if err := client.WriteMessage(websocket.TextMessage, messageJSON); err != nil {
			log.Println("Error sending WebSocket message:", err)
			delete(wm.clients, chatID) // Remove client if write fails
			client.Close()             // Close the problematic connection
		}
	}
}

// PublishControlCommand sends a control command to the WebSocket client for a chat ID
func (wm *WebSocketManager) PublishControlCommand(chatID int64, command string, value interface{}) {
	if client, ok := wm.clients[chatID]; ok {
		msg := map[string]interface{}{
			"command": command,
			"value":   value,
		}
		messageJSON, err := json.Marshal(msg)
		if err != nil {
			log.Println("Error marshalling control message:", err)
			return
		}
		if err := client.WriteMessage(websocket.TextMessage, messageJSON); err != nil {
			log.Println("Error sending WebSocket control message:", err)
			delete(wm.clients, chatID) // Remove client if write fails
			client.Close()             // Close the problematic connection
		}
	}
}

// handleWebSocket manages WebSocket connections and adds authorization
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	chatID, err := parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		if s.config.DebugMode {
			s.logger.Printf("[DEBUG] WebSocket: Invalid chat ID in request from %s", r.RemoteAddr)
		}
		return
	}

	if s.config.DebugMode {
		s.logger.Printf("[DEBUG] WebSocket connection attempt from %s for chat ID %d", r.RemoteAddr, chatID)
	}

	// Authorize user based on chatID (assuming chatID from URL is the user's ID in private chat)
	userInfo, err := s.userRepository.GetUserInfo(chatID)
	if err != nil || !userInfo.IsAuthorized {
		http.Error(w, "Unauthorized WebSocket connection: User not found or not authorized.", http.StatusUnauthorized)
		s.logger.Printf("Unauthorized WebSocket connection attempt for chatID %d: User not found or not authorized (%v)", chatID, err)
		if s.config.DebugMode {
			s.logger.Printf("[DEBUG] WebSocket: Authorization failed for chat ID %d from %s", chatID, r.RemoteAddr)
		}
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer ws.Close()

	s.wsManager.AddClient(chatID, ws)
	s.logger.Printf("WebSocket client connected for chat ID: %d", chatID)

	for {
		// Keep the connection alive or handle control messages.
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			log.Println("WebSocket read error:", err)
			s.wsManager.RemoveClient(chatID)
			break
		}
		// Echo the message back (optional, for keeping the connection alive).
		if err := ws.WriteMessage(messageType, p); err != nil {
			log.Println("WebSocket write error:", err)
			break
		}
	}
	s.logger.Printf("WebSocket client disconnected for chat ID: %d", chatID)
}

// parseChatID parses chat ID from request variables
func parseChatID(vars map[string]string) (int64, error) {
	chatIDStr, ok := vars["chatID"]
	if !ok {
		return 0, http.ErrMissingFile // Reusing error for simplicity
	}
	return strconv.ParseInt(chatIDStr, 10, 64)
}
