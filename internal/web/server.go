package web

import (
	"fmt"
	"net/http"
	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/logger"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/ext"
	"github.com/gorilla/mux"
)

const (
	tmplPath = "templates/player.html"
)

// Server represents the web server for streaming media and WebSocket connections
type Server struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	wsManager      *WebSocketManager
}

// NewServer creates a new web server instance
func NewServer(
	config *config.Configuration,
	tgClient *gotgproto.Client,
	tgCtx *ext.Context,
	log *logger.Logger,
	userRepository *data.UserRepository,
) *Server {
	return &Server{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepository,
		wsManager:      NewWebSocketManager(),
	}
}

// Start starts the web server with all routes configured
func (s *Server) Start() {
	router := mux.NewRouter()

	// Register routes
	router.HandleFunc("/ws/{chatID}", s.handleWebSocket)
	router.HandleFunc("/avatar/{chatID}", s.handleAvatar)
	router.HandleFunc("/api/validate-user/{chatID}", s.handleValidateUser)
	router.HandleFunc("/proxy", s.handleProxy)
	router.HandleFunc("/{messageID}/{hash}", s.handleStream)
	router.HandleFunc("/{chatID}", s.handlePlayer)
	router.HandleFunc("/{chatID}/", s.handlePlayer)

	s.logger.Infof("Web server started on port %s", s.config.Port)
	if err := http.ListenAndServe(fmt.Sprintf(":%s", s.config.Port), router); err != nil {
		s.logger.Fatalf("Failed to start web server: %v", err)
	}
}

// GetWSManager returns the WebSocket manager for publishing messages
func (s *Server) GetWSManager() *WebSocketManager {
	return s.wsManager
}
