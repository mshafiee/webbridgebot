package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/reader"

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/storage"
	"github.com/glebarez/sqlite"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/gotd/td/tg"
)

const (
	callbackResendToPlayer = "cb_ResendToPlayer"
	// New control action callbacks
	callbackPlay             = "cb_Play" // Will be used for Play/Pause toggle
	callbackRestart          = "cb_Restart"
	callbackForward10        = "cb_Fwd10"
	callbackBackward10       = "cb_Bwd10"
	callbackToggleFullscreen = "cb_ToggleFullscreen" // NEW
	callbackListUsers        = "cb_listusers"        // For pagination of /listusers command
	callbackUserAuthAction   = "cb_user_auth_action" // For user authorization/decline buttons
	tmplPath                 = "templates/player.html"
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
func NewTelegramBot(config *config.Configuration, logger *log.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)
	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			Session:          sessionMaker.SqlSession(sqlite.Open(dsn)),
			DisableCopyright: true,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Telegram client: %w", err)
	}

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
	clientDispatcher.AddHandler(handlers.NewCommand("deauthorize", b.handleDeauthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	clientDispatcher.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	clientDispatcher.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("cb_"), b.handleCallbackQuery)) // Catch all cb_ prefixes
	clientDispatcher.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))                                              // Can be removed for production to reduce noise
	// These filters handle both forwarded and directly uploaded media
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Audio, b.handleMediaMessages))
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Video, b.handleMediaMessages))
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Photo, b.handleMediaMessages)) // Now fully supported
}

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	// IMPORTANT: Prevent the bot from authorizing itself or counting itself as the first user.
	// This can happen during bot startup/initialization where ctx.Self.ID might trigger a /start-like update.
	if user.ID == ctx.Self.ID {
		b.logger.Printf("Ignoring /start command from bot's own ID (%d).", user.ID)
		return nil // Do not process updates from the bot itself for user management
	}

	b.logger.Printf("Processing /start command from user: %s (ID: %d) in chat: %d\n", user.FirstName, user.ID, chatID)

	existingUser, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			b.logger.Printf("User %d not found in DB, attempting to register.", user.ID)
			existingUser = nil
		} else {
			b.logger.Printf("Failed to retrieve user info from DB for /start: %v", err)
			return fmt.Errorf("failed to retrieve user info for start command: %w", err)
		}
	}

	isFirstUser, err := b.userRepository.IsFirstUser()
	if err != nil {
		b.logger.Printf("Failed to check if user is first: %v", err)
		return fmt.Errorf("failed to check first user status: %w", err)
	}

	isAdmin := false
	isAuthorized := false

	if existingUser == nil {
		if isFirstUser {
			isAuthorized = true
			isAdmin = true
			b.logger.Printf("User %d is the first user and has been automatically granted admin rights.", user.ID)
		}

		err = b.userRepository.StoreUserInfo(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin)
		if err != nil {
			b.logger.Printf("Failed to store user info for new user %d: %v", user.ID, err)
			return fmt.Errorf("failed to store user info: %w", err)
		}
		b.logger.Printf("Stored new user %d with isAuthorized=%t, isAdmin=%t", user.ID, isAuthorized, isAdmin)

		if !isAdmin {
			go b.notifyAdminsAboutNewUser(user, chatID)
		}
	} else {
		isAuthorized = existingUser.IsAuthorized
		isAdmin = existingUser.IsAdmin
		b.logger.Printf("User %d already exists in DB with isAuthorized=%t, isAdmin=%t", user.ID, isAuthorized, isAdmin)
	}

	webURL := fmt.Sprintf("%s/%d", b.config.BaseURL, chatID)
	startMsg := fmt.Sprintf(
		"Hello %s, I am @%s, your bridge between Telegram and the Web!\n"+
			"You can forward or directly upload media to this bot, and I will play it on your web player instantly.\n"+
			"Click on 'Open Web URL' below or access your player here: %s",
		user.FirstName, ctx.Self.Username, webURL,
	)
	err = b.sendMediaURLReply(ctx, u, startMsg, webURL)
	if err != nil {
		b.logger.Printf("Failed to send start message to user %d: %v", user.ID, err)
		return fmt.Errorf("failed to send start message: %w", err)
	}

	if !isAuthorized {
		b.logger.Printf("DEBUG: User %d is NOT authorized (isAuthorized=%t). Sending unauthorized message for media.", user.ID, isAuthorized) // Added DEBUG log
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}
	b.logger.Printf("DEBUG: User %d is authorized. /start command completed successfully.", user.ID) // Added DEBUG log
	return nil
}

// notifyAdminsAboutNewUser sends a notification to all admins about the new user.
func (b *TelegramBot) notifyAdminsAboutNewUser(newUser *tg.User, newUsersChatID int64) {
	admins, err := b.userRepository.GetAllAdmins()
	if err != nil {
		b.logger.Printf("Failed to retrieve admin list: %v", err)
		return
	}

	var notificationMsg string
	username, hasUsername := newUser.GetUsername()
	if hasUsername {
		// Do not escape here, Telegram will parse MarkdownV2 if ParseMode is set
		notificationMsg = fmt.Sprintf("A new user has joined: *@%s* (%s %s)\nID: `%d`\n\n_Use the buttons below to manage authorization._", username, newUser.FirstName, newUser.LastName, newUser.ID)
	} else {
		notificationMsg = fmt.Sprintf("A new user has joined: %s %s\nID: `%d`\n\n_Use the buttons below to manage authorization._", newUser.FirstName, newUser.LastName, newUser.ID)
	}

	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "✅ Authorize", Data: []byte(fmt.Sprintf("%s,%d,authorize", callbackUserAuthAction, newUser.ID))},
					&tg.KeyboardButtonCallback{Text: "❌ Decline", Data: []byte(fmt.Sprintf("%s,%d,decline", callbackUserAuthAction, newUser.ID))},
				},
			},
		},
	}

	for _, admin := range admins {
		// Avoid notifying the new user if they happened to be the first admin
		// Also avoid notifying the new user if the admin who triggered the update is the new user
		if admin.UserID == newUser.ID && admin.UserID == newUsersChatID {
			continue
		}
		b.logger.Printf("Notifying admin %d about new user %d", admin.UserID, newUser.ID)

		// Directly send the message with ParseMode set to MarkdownV2
		_, err := b.tgCtx.SendMessage(admin.ChatID, &tg.MessagesSendMessageRequest{
			Message:     notificationMsg,
			ReplyMarkup: markup,
		})
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

	adminMsgSuffix := ""
	if isAdmin {
		adminMsgSuffix = " as an admin"
	}
	// Notify the target user
	targetUserInfo, err := b.userRepository.GetUserInfo(targetUserID)
	if err == nil {
		b.tgCtx.SendMessage(targetUserInfo.ChatID, &tg.MessagesSendMessageRequest{Message: fmt.Sprintf("You have been authorized%s to use WebBridgeBot!", adminMsgSuffix)})
	} else {
		b.logger.Printf("Could not send notification to authorized user %d: %v", targetUserID, err)
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been authorized%s.", targetUserID, adminMsgSuffix))
}

func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
	// Only allow admins to run this command
	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil {
		b.logger.Printf("Failed to retrieve user info for admin check: %v", err)
		return b.sendReply(ctx, u, "Failed to deauthorize the user.")
	}

	if !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	// Parse the user ID from the command
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /deauthorize <user_id>")
	}
	targetUserID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	// Deauthorize the user
	err = b.userRepository.DeauthorizeUser(targetUserID)
	if err != nil {
		b.logger.Printf("Failed to deauthorize user %d: %v", targetUserID, err)
		return b.sendReply(ctx, u, "Failed to deauthorize the user.")
	}

	// Notify the target user
	targetUserInfo, err := b.userRepository.GetUserInfo(targetUserID)
	if err == nil {
		b.tgCtx.SendMessage(targetUserInfo.ChatID, &tg.MessagesSendMessageRequest{Message: "You have been deauthorized from using WebBridgeBot."})
	} else {
		b.logger.Printf("Could not send notification to deauthorized user %d: %v", targetUserID, err)
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been deauthorized.", targetUserID))
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	// This handler is useful for debugging to see all incoming updates.
	// Uncomment the following lines for detailed update logging:
	/*
		b.logger.Printf("Received update: %T", u.Update)
		if u.EffectiveMessage != nil {
			b.logger.Printf("Effective message from user %d in chat %d: %s", u.EffectiveUser().ID, u.EffectiveChat().GetID(), u.EffectiveMessage.Text)
			if u.EffectiveMessage.Message.Media != nil {
				b.logger.Printf("Media type: %T", u.EffectiveMessage.Message.Media)
			}
		}
	*/
	return nil
}

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID() // This will correctly be the forwarding user's ID in a private chat
	user := u.EffectiveUser()           // This might be the original sender's ID for forwarded messages

	b.logger.Printf("Processing media message from user: %s (ID: %d) in chat: %d", user.FirstName, user.ID, chatID)

	if !b.isUserChat(ctx, chatID) {
		return dispatcher.EndGroups // Only process media from private chats
	}

	existingUser, err := b.userRepository.GetUserInfo(chatID)
	if err != nil {
		if err == sql.ErrNoRows { // User not in DB
			b.logger.Printf("User %d not in DB for media message, sending unauthorized message.", chatID)
			authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
			return b.sendReply(ctx, u, authorizationMsg)
		}
		b.logger.Printf("Failed to retrieve user info from DB for media message for user %d: %v", chatID, err)
		return fmt.Errorf("failed to retrieve user info for media handling: %w", err)
	}

	b.logger.Printf("User %d retrieved for media message. isAuthorized=%t, isAdmin=%t", chatID, existingUser.IsAuthorized, existingUser.IsAdmin)

	if !existingUser.IsAuthorized {
		b.logger.Printf("DEBUG: User %d is NOT authorized (isAuthorized=%t). Sending unauthorized message for media.", chatID, existingUser.IsAuthorized)
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}

	// Check if media type is supported and extract DocumentFile
	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		b.logger.Printf("Error processing media message from chat ID %d, message ID %d: %v", chatID, u.EffectiveMessage.Message.ID, err)
		return b.sendReply(ctx, u, fmt.Sprintf("Unsupported media type or error processing file: %v", err))
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
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{}) // Use ReplyTextString for plain text
	if err != nil {
		b.logger.Printf("Failed to send reply to user: %s (ID: %d) - Error: %v", u.EffectiveUser().FirstName, u.EffectiveUser().ID, err)
	}
	return err
}

func (b *TelegramBot) sendMediaURLReply(ctx *ext.Context, u *ext.Update, msg, webURL string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{ // Use ReplyTextString for plain text
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
	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{ // Use ReplyTextString for plain text
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
				{
					Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonCallback{Text: "Toggle Fullscreen", Data: []byte(callbackToggleFullscreen)},
					},
				},
				{
					Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonCallback{Text: "▶️/⏸️", Data: []byte(callbackPlay)},
						&tg.KeyboardButtonCallback{Text: "🔄", Data: []byte(callbackRestart)},
						&tg.KeyboardButtonCallback{Text: "⏪ 10s", Data: []byte(callbackBackward10)},
						&tg.KeyboardButtonCallback{Text: "⏩ 10s", Data: []byte(callbackForward10)},
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
		"fileId":   strconv.FormatInt(file.ID, 10), // Use FormatInt for int64
		"mimeType": file.MimeType,
		"duration": strconv.Itoa(file.Duration),
		"width":    strconv.Itoa(file.Width),
		"height":   strconv.Itoa(file.Height),
	}
}

// publishControlCommandToWebSocket sends a control command to the specified chat's WebSocket client.
func (b *TelegramBot) publishControlCommandToWebSocket(chatID int64, command string, value interface{}) {
	if client, ok := wsClients[chatID]; ok {
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
			delete(wsClients, chatID) // Remove client if write fails
			client.Close()            // Close the problematic connection
		}
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
			delete(wsClients, chatID) // Remove client if write fails
			client.Close()            // Close the problematic connection
		}
	}
}

func (b *TelegramBot) handleCallbackQuery(ctx *ext.Context, u *ext.Update) error {
	// Check for user authorization/decline callbacks first
	if strings.HasPrefix(string(u.CallbackQuery.Data), callbackUserAuthAction) {
		dataParts := strings.Split(string(u.CallbackQuery.Data), ",")
		if len(dataParts) < 3 {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Invalid user authorization callback data.",
			})
			return nil
		}

		targetUserID, err := strconv.ParseInt(dataParts[1], 10, 64)
		if err != nil {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Invalid user ID in callback data.",
			})
			return nil
		}
		actionType := dataParts[2]

		// Ensure the current user is an admin
		adminID := u.EffectiveUser().ID
		adminUserInfo, err := b.userRepository.GetUserInfo(adminID)
		if err != nil || !adminUserInfo.IsAdmin {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "You are not authorized to perform this action.",
			})
			return nil
		}

		targetUserInfo, err := b.userRepository.GetUserInfo(targetUserID)
		if err != nil {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Target user not found.",
			})
			return nil
		}

		var (
			adminResponseMessage    string
			userNotificationMessage string
		)

		switch actionType {
		case "authorize":
			if targetUserInfo.IsAuthorized {
				adminResponseMessage = fmt.Sprintf("User %d is already authorized.", targetUserID)
			} else {
				err = b.userRepository.AuthorizeUser(targetUserID, false) // Authorize, but not as admin via this button
				if err != nil {
					b.logger.Printf("Failed to authorize user %d via callback: %v", targetUserID, err)
					adminResponseMessage = fmt.Sprintf("Failed to authorize user %d.", targetUserID)
				} else {
					adminResponseMessage = fmt.Sprintf("User %d authorized successfully.", targetUserID)
					userNotificationMessage = "You have been authorized to use WebBridgeBot!"
				}
			}
		case "decline":
			if !targetUserInfo.IsAuthorized {
				adminResponseMessage = fmt.Sprintf("User %d is already deauthorized.", targetUserID)
			} else {
				err = b.userRepository.DeauthorizeUser(targetUserID) // Deauthorize, also removes admin status
				if err != nil {
					b.logger.Printf("Failed to deauthorize user %d via callback: %v", targetUserID, err)
					adminResponseMessage = fmt.Sprintf("Failed to deauthorize user %d.", targetUserID)
				} else {
					adminResponseMessage = fmt.Sprintf("User %d deauthorized successfully.", targetUserID)
					userNotificationMessage = "Your request to use WebBridgeBot has been declined by an administrator."
				}
			}
		default:
			adminResponseMessage = "Unknown authorization action."
		}

		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
			QueryID: u.CallbackQuery.QueryID,
			Message: adminResponseMessage,
		})

		if userNotificationMessage != "" {
			_, err := b.tgCtx.SendMessage(targetUserInfo.ChatID, &tg.MessagesSendMessageRequest{Message: userNotificationMessage})
			if err != nil {
				b.logger.Printf("Failed to send notification to user %d: %v", targetUserID, err)
			}
		}
		return nil
	}

	// Then, check for `cb_ResendToPlayer` which has a specific format with message ID.
	if strings.HasPrefix(string(u.CallbackQuery.Data), callbackResendToPlayer) {
		dataParts := strings.Split(string(u.CallbackQuery.Data), ",")
		if len(dataParts) > 1 {
			messageID, err := strconv.Atoi(dataParts[1])
			if err != nil {
				_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
					QueryID: u.CallbackQuery.QueryID,
					Message: "Invalid message ID in callback data.",
				})
				return nil
			}

			file, err := utils.FileFromMessage(ctx, b.tgClient, messageID)
			if err != nil {
				b.logger.Printf("Error fetching file for message ID %d for callback: %v", messageID, err)
				_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
					QueryID: u.CallbackQuery.QueryID,
					Message: "Failed to retrieve file info.",
				})
				return nil
			}

			wsMsg := b.constructWebSocketMessage(b.generateFileURL(messageID, file), file)
			b.publishToWebSocket(u.EffectiveChat().GetID(), wsMsg)

			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				Alert:   false,
				QueryID: u.CallbackQuery.QueryID,
				Message: fmt.Sprintf("The %s file has been sent to the web player.", file.FileName),
			})
			return nil
		}
	}

	// For other simple commands, they are simple string matches.
	callbackType := string(u.CallbackQuery.Data)
	chatID := u.EffectiveChat().GetID() // The chat ID to which the web player is linked.

	switch callbackType {
	case callbackPlay:
		if _, ok := wsClients[chatID]; ok { // Check if a websocket is connected for this chatID
			b.publishControlCommandToWebSocket(chatID, "togglePlayPause", nil) // Client side will toggle
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Playback toggled.",
			})
		} else {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Web player not connected.",
			})
		}
	case callbackRestart:
		if _, ok := wsClients[chatID]; ok {
			b.publishControlCommandToWebSocket(chatID, "restart", nil)
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Restarting media.",
			})
		} else {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Web player not connected.",
			})
		}
	case callbackForward10:
		if _, ok := wsClients[chatID]; ok {
			b.publishControlCommandToWebSocket(chatID, "seek", 10)
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Forwarded 10 seconds.",
			})
		} else {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Web player not connected.",
			})
		}
	case callbackBackward10:
		if _, ok := wsClients[chatID]; ok {
			b.publishControlCommandToWebSocket(chatID, "seek", -10)
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Rewound 10 seconds.",
			})
		} else {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Web player not connected.",
			})
		}
	case callbackToggleFullscreen: // NEW: Handle the toggle fullscreen callback
		if _, ok := wsClients[chatID]; ok {
			b.publishControlCommandToWebSocket(chatID, "toggleFullscreen", nil) // Send command to WebSocket
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Fullscreen toggled.",
			})
		} else {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Web player not connected.",
			})
		}
	case callbackListUsers:
		// Handle pagination for /listusers. The data format is "cb_listusers,PAGE_NUMBER"
		dataParts := strings.Split(string(u.CallbackQuery.Data), ",")
		if len(dataParts) < 2 {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Invalid callback data for listusers pagination.",
			})
			return nil
		}
		page, err := strconv.Atoi(dataParts[1])
		if err != nil {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Invalid page number.",
			})
			return nil
		}
		// Store original text
		originalMessageText := u.EffectiveMessage.Text
		// Modify the text of the existing EffectiveMessage to simulate the command
		// This is a common pattern for re-dispatching to command handlers
		u.EffectiveMessage.Text = fmt.Sprintf("/listusers %d", page)
		// Call the handler directly with the *original* update object, which now has modified text
		err = b.handleListUsers(ctx, u)
		// Restore original text immediately after, in case 'u' object is reused by dispatcher.
		u.EffectiveMessage.Text = originalMessageText

		if err != nil {
			b.logger.Printf("Error processing listusers callback: %v", err)
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Error loading users.",
			})
		} else {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Users list updated.",
			})
		}
		return nil

	default:
		// Fallback for unknown callback queries, or if `cb_ResendToPlayer` didn't match.
		b.logger.Printf("Unknown callback query received: %s", u.CallbackQuery.Data)
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
			QueryID: u.CallbackQuery.QueryID,
			Message: "Unknown action.",
		})
		return nil
	}
	return nil
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

// handleWebSocket manages WebSocket connections and adds authorization.
func (b *TelegramBot) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	chatID, err := b.parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Authorize user based on chatID (assuming chatID from URL is the user's ID in private chat)
	userInfo, err := b.userRepository.GetUserInfo(chatID)
	if err != nil || !userInfo.IsAuthorized {
		http.Error(w, "Unauthorized WebSocket connection: User not found or not authorized.", http.StatusUnauthorized)
		b.logger.Printf("Unauthorized WebSocket connection attempt for chatID %d: User not found or not authorized (%v)", chatID, err) // Added detailed log
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer ws.Close()

	wsClients[chatID] = ws
	b.logger.Printf("WebSocket client connected for chat ID: %d", chatID)

	for {
		// Keep the connection alive or handle control messages.
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			log.Println("WebSocket read error:", err)
			delete(wsClients, chatID)
			break
		}
		// Echo the message back (optional, for keeping the connection alive).
		if err := ws.WriteMessage(messageType, p); err != nil {
			log.Println("WebSocket write error:", err)
			break
		}
	}
	b.logger.Printf("WebSocket client disconnected for chat ID: %d", chatID)
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

	// Fetch the file information from Telegram (or cache)
	file, err := utils.FileFromMessage(ctx, b.tgClient, messageID)
	if err != nil {
		b.logger.Printf("Error fetching file for message ID %d: %v", messageID, err)
		http.Error(w, "Unable to retrieve file for the specified message", http.StatusBadRequest)
		return
	}

	// Hash verification
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
	lr, err := reader.NewTelegramReader(ctx, b.tgClient, file.Location, start, end, contentLength, b.config.BinaryCache, b.logger)
	if err != nil {
		b.logger.Printf("Error creating Telegram reader for message ID %d: %v", messageID, err)
		http.Error(w, "Failed to initialize file stream", http.StatusInternalServerError)
		return
	}
	defer lr.Close()

	// Send appropriate headers and stream the content.
	if rangeHeader != "" {
		b.logger.Printf("Serving partial content for message ID %d: bytes %d-%d/%d", messageID, start, end, contentLength)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, contentLength))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("Content-Type", file.MimeType) // Use actual mime type from file
		w.WriteHeader(http.StatusPartialContent)
	} else {
		b.logger.Printf("Serving full content for message ID %d", messageID)
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
		w.Header().Set("Content-Type", file.MimeType) // Use actual mime type from file
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, file.FileName))
	}

	// Stream the content to the client.
	if _, err := io.Copy(w, lr); err != nil {
		b.logger.Printf("Error streaming content for message ID %d: %v", messageID, err)
		// Headers might already be sent, so just log the error.
	}
}

func (b *TelegramBot) parseChatID(vars map[string]string) (int64, error) {
	chatIDStr, ok := vars["chatID"]
	if !ok {
		return 0, fmt.Errorf("Chat ID is required")
	}

	return strconv.ParseInt(chatIDStr, 10, 64)
}

// handlePlayer serves the HTML player page and adds authorization.
func (b *TelegramBot) handlePlayer(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request for player: %s", r.URL.Path)

	chatID, err := b.parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Authorize user based on chatID (assuming chatID from URL is the user's ID in private chat)
	userInfo, err := b.userRepository.GetUserInfo(chatID)
	if err != nil || !userInfo.IsAuthorized {
		http.Error(w, "Unauthorized access to player. Please start the bot first.", http.StatusUnauthorized)
		b.logger.Printf("Unauthorized player access attempt for chatID %d: User not found or not authorized (%v)", chatID, err) // Added detailed log
		return
	}

	t, err := template.ParseFiles(tmplPath)
	if err != nil {
		b.logger.Printf("Error loading template: %v", err)
		http.Error(w, "Failed to load template", http.StatusInternalServerError)
		return
	}

	if err := t.Execute(w, map[string]interface{}{"User": userInfo}); err != nil {
		b.logger.Printf("Error rendering template: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
	}
}

// handleListUsers lists all users in a paginated format
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	const pageSize = 10
	page := 1
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) > 1 {
		parsedPage, err := strconv.Atoi(args[1])
		if err == nil && parsedPage > 0 {
			page = parsedPage
		}
	}

	totalUsers, err := b.userRepository.GetUserCount()
	if err != nil {
		b.logger.Printf("Failed to get user count: %v", err)
		return b.sendReply(ctx, u, "Error retrieving user count.")
	}

	offset := (page - 1) * pageSize
	users, err := b.userRepository.GetAllUsers(offset, pageSize)
	if err != nil {
		b.logger.Printf("Failed to get users for listing: %v", err)
		return b.sendReply(ctx, u, "Error retrieving user list.")
	}

	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users found or page is empty.")
	}

	var msg strings.Builder
	msg.WriteString("👥 *User List*\n\n")
	for i, user := range users {
		status := "❌"
		if user.IsAuthorized {
			status = "✅"
		}
		adminStatus := ""
		if user.IsAdmin {
			adminStatus = "👑"
		}
		username := user.Username
		if username == "" {
			username = "N/A"
		}
		// Do not escape here, Telegram will parse MarkdownV2 if ParseMode is set
		msg.WriteString(fmt.Sprintf("%d\\. `ID:%d` %s %s (`@%s`) - Auth: %s Admin: %s\n",
			offset+i+1, user.UserID, user.FirstName, user.LastName, username, status, adminStatus))
	}

	totalPages := (totalUsers + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage *%d* of *%d* \\(%d total users\\)", page, totalPages, totalUsers))

	markup := &tg.ReplyInlineMarkup{}
	var buttons []tg.KeyboardButtonClass
	if page > 1 {
		buttons = append(buttons, &tg.KeyboardButtonCallback{
			Text: "⬅️ Prev",
			Data: []byte(fmt.Sprintf("%s,%d", callbackListUsers, page-1)),
		})
	}
	if page < totalPages {
		buttons = append(buttons, &tg.KeyboardButtonCallback{
			Text: "Next ➡️",
			Data: []byte(fmt.Sprintf("%s,%d", callbackListUsers, page+1)),
		})
	}
	if len(buttons) > 0 {
		markup.Rows = append(markup.Rows, tg.KeyboardButtonRow{Buttons: buttons})
	}

	// Correct way to send styled text with ctx.Reply using ParseMode
	_, err = ctx.Reply(u, ext.ReplyTextString(msg.String()), &ext.ReplyOpts{
		Markup: markup,
	})
	return err
}

// handleUserInfo retrieves detailed information about a specific user.
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}
	targetUserID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	targetUserInfo, err := b.userRepository.GetUserInfo(targetUserID)
	if err != nil {
		if err == sql.ErrNoRows {
			return b.sendReply(ctx, u, fmt.Sprintf("User with ID %d not found.", targetUserID))
		}
		b.logger.Printf("Failed to get user info for ID %d: %v", targetUserID, err)
		return b.sendReply(ctx, u, "Error retrieving user information.")
	}

	status := "Not Authorized ❌"
	if targetUserInfo.IsAuthorized {
		status = "Authorized ✅"
	}
	adminStatus := "No 🚫"
	if targetUserInfo.IsAdmin {
		adminStatus = "Yes 👑"
	}

	username := targetUserInfo.Username
	if username == "" {
		username = "N/A"
	}

	// Do not escape here, Telegram will parse MarkdownV2 if ParseMode is set
	msg := fmt.Sprintf(
		"👤 *User Details:*\n"+
			"ID: `%d`\n"+
			"Chat ID: `%d`\n"+
			"First Name: `%s`\n"+
			"Last Name: `%s`\n"+
			"Username: `@%s`\n"+
			"Status: *%s*\n"+
			"Admin: *%s*\n"+
			"Joined: `%s`",
		targetUserInfo.UserID,
		targetUserInfo.ChatID,
		targetUserInfo.FirstName,
		targetUserInfo.LastName,
		username,
		status,
		adminStatus,
		targetUserInfo.CreatedAt,
	)

	// Correct way to send styled text with ctx.Reply using ParseMode
	_, err = ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

// escapeMarkdownV2 escapes characters that have special meaning in Telegram MarkdownV2.
// This function should be used for *literal* strings that are part of MarkdownV2 formatted message
// but should NOT be interpreted as Markdown syntax.
// If the entire message is intended to be MarkdownV2 and sent with ParseMode: "MarkdownV2",
// then only the parts that need to be literal should be escaped.
func escapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(text)
}
