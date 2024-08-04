package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/reader"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/storage"
	gtypes "github.com/celestix/gotgproto/types"
	"github.com/glebarez/sqlite"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/gotd/td/tg"
	"webBridgeBot/internal/config"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
)

const (
	callbackResendToPlayer = "cb_ResendToPlayer"
	tmplPath               = "templates/player.html"
)

// TelegramBot represents the main bot structure.
type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *log.Logger
	userRepository *data.UserRepository
	db             *sql.DB
}

var (
	wsClients = make(map[int64]*websocket.Conn)

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
)

// NewTelegramBot creates a new instance of TelegramBot.
func NewTelegramBot(config *config.Configuration) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)
	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory: true,
			Session:  sessionMaker.SqlSession(sqlite.Open(dsn)),
		})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Telegram client: %w", err)
	}

	logger := log.New(os.Stdout, "TelegramBot: ", log.Ldate|log.Ltime|log.Lshortfile)

	// Initialize the database connection
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	// Create a new UserRepository
	userRepository := data.NewUserRepository(db)

	// Initialize the database schema
	if err := userRepository.InitDB(); err != nil {
		return nil, err
	}

	return &TelegramBot{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgClient.CreateContext(),
		logger:         logger,
		userRepository: userRepository,
		db:             db,
	}, nil
}

// Run starts the Telegram bot and web server.
func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)

	b.registerHandlers()

	go b.startWebServer()

	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	clientDispatcher := b.tgClient.Dispatcher
	clientDispatcher.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	clientDispatcher.AddHandler(handlers.NewCommand("authorize", b.handleAuthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("cb_"), b.handleCallbackQuery))
	clientDispatcher.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Video, b.handleVideoMessages))
}
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	b.logger.Printf("Processing /start command from user: %s (ID: %d) in chat: %d\n", user.FirstName, user.ID, chatID)

	// Check if the user already exists in the database
	existingUser, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil {
		b.logger.Printf("Failed to retrieve user info: %v", err)
	}

	// Check if the user is the first user in the database
	isFirstUser, err := b.userRepository.IsFirstUser()
	if err != nil {
		b.logger.Printf("Failed to check if user is first: %v", err)
	}

	isAdmin := false
	isAuthorized := false

	// If the user doesn't exist or is the first user, store user info or update their record
	if existingUser == nil {
		if isFirstUser {
			isAuthorized = true
			isAdmin = true
			b.logger.Printf("User %d is the first user and has been automatically granted admin rights.", user.ID)
		}

		err = b.userRepository.StoreUserInfo(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin)
		if err != nil {
			b.logger.Printf("Failed to store user info: %v", err)
		}

		// Notify admins if the user is not an admin
		if !isAdmin {
			go b.notifyAdminsAboutNewUser(user)
		}
	} else {
		isAuthorized = existingUser.IsAuthorized
		isAdmin = existingUser.IsAdmin
	}

	// Send the start message to the user
	webURL := fmt.Sprintf("%s/%d", b.config.BaseURL, chatID)
	startMsg := fmt.Sprintf(
		"Hello %s, I am @%s, your bridge between Telegram and the Web!\n"+
			"You can forward media to this bot, and I will play it on your web player instantly.\n"+
			"Click on 'Open Web URL' below or access your player here: %s",
		user.FirstName, ctx.Self.Username, webURL,
	)
	err = b.sendMediaURLReply(ctx, u, startMsg, webURL)
	if err != nil {
		b.logger.Printf("Failed to send start message: %v", err)
	}

	// If the user is not authorized, send an additional message informing them
	if !isAuthorized {
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}

	return nil
}

// notifyAdminsAboutNewUser sends a notification to all admins about the new user.
func (b *TelegramBot) notifyAdminsAboutNewUser(newUser *tg.User) {
	admins, err := b.userRepository.GetAllAdmins()
	if err != nil {
		b.logger.Printf("Failed to retrieve admin list: %v", err)
		return
	}

	var notificationMsg string
	if username, hasUsername := newUser.GetUsername(); hasUsername {
		notificationMsg = fmt.Sprintf("A new user has joined: @%s %s %s\nID: %d\nUse this command: /authorize %d", username, newUser.FirstName, newUser.LastName, newUser.ID, newUser.ID)
	} else {
		notificationMsg = fmt.Sprintf("A new user has joined: %s %s\nID: %d\nUse this command: /authorize %d", newUser.FirstName, newUser.LastName, newUser.ID, newUser.ID)
	}

	for _, admin := range admins {
		b.logger.Printf("Notifying admin %d about new user %d", admin.UserID, newUser.ID)
		_, err := b.tgCtx.SendMessage(admin.ChatID, &tg.MessagesSendMessageRequest{Message: notificationMsg})
		if err != nil {
			b.logger.Printf("Failed to notify admin %d: %v", admin.UserID, err)
		}
	}
}

func (b *TelegramBot) handleAuthorizeUser(ctx *ext.Context, u *ext.Update) error {
	// Only allow admins to run this command
	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil {
		b.logger.Printf("Failed to retrieve user info for admin check: %v", err)
		return b.sendReply(ctx, u, "Failed to authorize the user.")
	}

	if !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	// Parse the user ID and optional admin flag from the command
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /authorize <user_id> [admin]")
	}
	targetUserID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	isAdmin := len(args) > 2 && args[2] == "admin"

	// Authorize the user and optionally promote to admin
	err = b.userRepository.AuthorizeUser(targetUserID, isAdmin)
	if err != nil {
		b.logger.Printf("Failed to authorize user %d: %v", targetUserID, err)
		return b.sendReply(ctx, u, "Failed to authorize the user.")
	}

	adminMsg := ""
	if isAdmin {
		adminMsg = " as an admin"
	}
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been authorized%s.", targetUserID, adminMsg))
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	return nil
}

func (b *TelegramBot) handleVideoMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	b.logger.Printf("Processing video message for chat ID: %d", chatID)

	if !b.isUserChat(ctx, chatID) {
		return dispatcher.EndGroups
	}

	user := u.EffectiveUser()

	existingUser, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil {
		return fmt.Errorf("failed to retrieve user info: %v", err)
	}

	if !existingUser.IsAuthorized {
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}

	if supported, err := isSupportedMedia(u.EffectiveMessage); !supported || err != nil {
		b.logger.Printf("Unsupported media type received in chat ID %d", chatID)
		return dispatcher.EndGroups
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		b.logger.Printf("Error extracting media file for chat ID %d, message ID %d: %v", u.EffectiveChat().GetID(), u.EffectiveMessage.Message.ID, err)
		return err
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	b.logger.Printf("Generated media file URL for message ID %d in chat ID %d: %s", u.EffectiveMessage.Message.ID, chatID, fileURL)

	return b.sendMediaToUser(ctx, u, fileURL, file)
}

func (b *TelegramBot) isUserChat(ctx *ext.Context, chatID int64) bool {
	peerChatID := ctx.PeerStorage.GetPeerById(chatID)
	if peerChatID.Type != int(storage.TypeUser) {
		b.logger.Printf("Chat ID %d is not a user type. Terminating processing.", chatID)
		return false
	}
	return true
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, msg, &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Failed to send reply to user: %s (ID: %d) - Error: %v", u.EffectiveUser().FirstName, u.EffectiveUser().ID, err)
	}
	return err
}

func (b *TelegramBot) sendMediaURLReply(ctx *ext.Context, u *ext.Update, msg, webURL string) error {
	_, err := ctx.Reply(u, msg, &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{
			Rows: []tg.KeyboardButtonRow{
				{
					Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonURL{Text: "Open Web URL", URL: webURL},
						&tg.KeyboardButtonURL{Text: "WebBridgeBot on GitHub", URL: "https://github.com/mshafiee/webbridgebot"},
					},
				},
			},
		},
	})
	if err != nil {
		b.logger.Printf("Failed to send reply to user: %s (ID: %d) - Error: %v", u.EffectiveUser().FirstName, u.EffectiveUser().ID, err)
	}
	return err
}

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile) error {
	_, err := ctx.Reply(u, fileURL, &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{
			Rows: []tg.KeyboardButtonRow{
				{
					Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonCallback{
							Text: "Resend to Player",
							Data: []byte(fmt.Sprintf("%s,%d", callbackResendToPlayer, u.EffectiveMessage.Message.ID)),
						},
						&tg.KeyboardButtonURL{Text: "Stream URL", URL: fileURL},
					},
				},
			},
		},
	})
	if err != nil {
		b.logger.Printf("Error sending reply for chat ID %d, message ID %d: %v", u.EffectiveChat().GetID(), u.EffectiveMessage.Message.ID, err)
		return err
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.publishToWebSocket(u.EffectiveChat().GetID(), wsMsg)
	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	return map[string]string{
		"url":      fileURL,
		"fileName": file.FileName,
		"fileId":   strconv.Itoa(int(file.ID)),
		"mimeType": file.MimeType,
		"duration": strconv.Itoa(int(file.VideoAttr.Duration)),
		"width":    strconv.Itoa(file.VideoAttr.W),
		"height":   strconv.Itoa(file.VideoAttr.H),
	}
}

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(
		file.FileName,
		file.FileSize,
		file.MimeType,
		file.ID,
	), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

func (b *TelegramBot) publishToWebSocket(chatID int64, message map[string]string) {
	if client, ok := wsClients[chatID]; ok {
		messageJSON, err := json.Marshal(message)
		if err != nil {
			log.Println("Error marshalling message:", err)
			return
		}
		if err := client.WriteMessage(websocket.TextMessage, messageJSON); err != nil {
			log.Println("Error sending WebSocket message:", err)
			delete(wsClients, chatID)
			client.Close()
		}
	}
}

func (b *TelegramBot) handleCallbackQuery(ctx *ext.Context, u *ext.Update) error {
	dataParts := strings.Split(string(u.CallbackQuery.Data), ",")
	if len(dataParts) > 0 && dataParts[0] == callbackResendToPlayer && len(dataParts) > 1 {
		messageID, err := strconv.Atoi(dataParts[1])
		if err != nil {
			return err
		}

		file, err := utils.FileFromMessage(ctx, b.tgClient, messageID)
		if err != nil {
			b.logger.Printf("Error fetching file for message ID %d: %v", messageID, err)
		}

		wsMsg := b.constructWebSocketMessage(b.generateFileURL(messageID, file), file)
		b.publishToWebSocket(u.EffectiveChat().GetID(), wsMsg)

		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
			Alert:   true,
			QueryID: u.CallbackQuery.QueryID,
			Message: fmt.Sprintf("The %s file has been sent to the web player.", file.FileName),
		})
	}
	return nil
}

func isSupportedMedia(m *gtypes.Message) (bool, error) {
	if m.Media == nil {
		return false, dispatcher.EndGroups
	}
	switch m.Media.(type) {
	case *tg.MessageMediaDocument:
		return true, nil
	default:
		return false, nil
	}
}

func (b *TelegramBot) startWebServer() {
	router := mux.NewRouter()

	router.HandleFunc("/ws/{chatID}", b.handleWebSocket)
	router.HandleFunc("/{messageID}/{hash}", b.handleStream)
	router.HandleFunc("/{chatID}", b.handlePlayer)
	router.HandleFunc("/{chatID}/", b.handlePlayer)

	log.Printf("Web server started on port %s", b.config.Port)
	if err := http.ListenAndServe(fmt.Sprintf(":%s", b.config.Port), router); err != nil {
		log.Panic(err)
	}
}

// handleWebSocket manages WebSocket connections.
func (b *TelegramBot) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	chatID, err := b.parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer ws.Close()

	// Register the WebSocket client.
	wsClients[chatID] = ws

	for {
		// Keep the connection alive or handle control messages.
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			log.Println(err)
			delete(wsClients, chatID)
			break
		}
		// Echo the message back (optional, for keeping the connection alive).
		if err := ws.WriteMessage(messageType, p); err != nil {
			log.Println(err)
			break
		}
	}
}

// handleStream handles the file streaming from Telegram.
func (b *TelegramBot) handleStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vars := mux.Vars(r)
	messageIDStr := vars["messageID"]
	authHash := vars["hash"]

	b.logger.Printf("Received request to stream file with message ID: %s from client %s", messageIDStr, r.RemoteAddr)

	// Parse and validate message ID.
	messageID, err := strconv.Atoi(messageIDStr)
	if err != nil {
		b.logger.Printf("Invalid message ID '%s' received from client %s", messageIDStr, r.RemoteAddr)
		http.Error(w, "Invalid message ID format", http.StatusBadRequest)
		return
	}

	// Fetch the file from Telegram.
	file, err := utils.FileFromMessage(ctx, b.tgClient, messageID)
	if err != nil {
		b.logger.Printf("Error fetching file for message ID %d: %v", messageID, err)
		http.Error(w, "Unable to retrieve file for the specified message", http.StatusBadRequest)
		return
	}

	expectedHash := utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID)
	if !utils.CheckHash(authHash, expectedHash, b.config.HashLength) {
		b.logger.Printf("Hash verification failed for message ID %d from client %s", messageID, r.RemoteAddr)
		http.Error(w, "Invalid authentication hash", http.StatusBadRequest)
		return
	}

	contentLength := file.FileSize

	// Default range values for full content.
	var start, end int64 = 0, contentLength - 1

	// Process range header if present.
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		b.logger.Printf("Range header received for message ID %d: %s", messageID, rangeHeader)
		if strings.HasPrefix(rangeHeader, "bytes=") {
			ranges := strings.Split(rangeHeader[len("bytes="):], "-")
			if len(ranges) == 2 {
				if ranges[0] != "" {
					start, err = strconv.ParseInt(ranges[0], 10, 64)
					if err != nil {
						b.logger.Printf("Invalid start range value for message ID %d: %v", messageID, err)
						http.Error(w, "Invalid range start value", http.StatusBadRequest)
						return
					}
				}
				if ranges[1] != "" {
					end, err = strconv.ParseInt(ranges[1], 10, 64)
					if err != nil {
						b.logger.Printf("Invalid end range value for message ID %d: %v", messageID, err)
						http.Error(w, "Invalid range end value", http.StatusBadRequest)
						return
					}
				}
			}
		}
	}

	// Validate the requested range.
	if start > end || start < 0 || end >= contentLength {
		b.logger.Printf("Requested range not satisfiable for message ID %d: start=%d, end=%d, contentLength=%d", messageID, start, end, contentLength)
		http.Error(w, "Requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Create a TelegramReader to stream the content.
	lr, err := reader.NewTelegramReader(ctx, b.tgClient, file.Location, start, end, contentLength, b.config.BinaryCache)
	if err != nil {
		b.logger.Printf("Error creating Telegram reader for message ID %d: %v", messageID, err)
		http.Error(w, "Failed to initialize file stream", http.StatusInternalServerError)
		return
	}
	defer lr.Close()

	// Send appropriate headers and stream the content.
	if rangeHeader != "" {
		b.logger.Printf("Serving partial content for message ID %d: bytes %d-%d of %d", messageID, start, end, contentLength)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, contentLength))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusPartialContent)
	} else {
		b.logger.Printf("Serving full content for message ID %d", messageID)
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, file.FileName))
	}

	// Stream the content to the client.
	if _, err := io.Copy(w, lr); err != nil {
		b.logger.Printf("Error streaming content for message ID %d: %v", messageID, err)
		http.Error(w, "Error streaming content", http.StatusInternalServerError)
	}
}

func (b *TelegramBot) parseChatID(vars map[string]string) (int64, error) {
	chatIDStr, ok := vars["chatID"]
	if !ok {
		return 0, fmt.Errorf("Chat ID is required")
	}

	return strconv.ParseInt(chatIDStr, 10, 64)
}

func (b *TelegramBot) handlePlayer(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request for player: %s", r.URL.Path)

	chatID, err := b.parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	t, err := template.ParseFiles(tmplPath)
	if err != nil {
		b.logger.Printf("Error loading template: %v", err)
		http.Error(w, "Failed to load template", http.StatusInternalServerError)
		return
	}

	if err := t.Execute(w, map[string]interface{}{"ChatID": chatID}); err != nil {
		b.logger.Printf("Error rendering template: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
	}
}
