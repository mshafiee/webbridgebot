package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/reader"
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
	// These filters handle both forwarded and directly uploaded media (including documents)
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages)) // Catches all media types
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

	b.logger.Printf("📥 Received /start command from user: %s (ID: %d) in chat: %d", user.FirstName, user.ID, chatID)

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] /start command - User: %s %s, Username: @%s, ChatID: %d",
			user.FirstName, user.LastName, user.Username, chatID)
	}

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
		"Hello %s, I am @%s, your bridge between Telegram and the Web!\n\n"+
			"📤 You can **forward** or **directly upload** media files (audio, video, photos, or documents) to this bot.\n"+
			"🎥 I will instantly generate a streaming link and play it on your web player.\n\n"+
			"✨ **Features:**\n"+
			"• Forward media from any chat\n"+
			"• Upload media directly (including video files as documents)\n"+
			"• Instant web streaming\n"+
			"• Control playback from Telegram\n\n"+
			"Click 'Open Web URL' below or access your player here: %s",
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
		notificationMsg = fmt.Sprintf("A new user has joined: *@%s* (%s %s)\nID: `%d`\n\n_Use the buttons below to manage authorization\\._", username, escapeMarkdownV2(newUser.FirstName), escapeMarkdownV2(newUser.LastName), newUser.ID)
	} else {
		notificationMsg = fmt.Sprintf("A new user has joined: %s %s\nID: `%d`\n\n_Use the buttons below to manage authorization\\._", escapeMarkdownV2(newUser.FirstName), escapeMarkdownV2(newUser.LastName), newUser.ID)
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

		peer := b.tgCtx.PeerStorage.GetInputPeerById(admin.ChatID)

		req := &tg.MessagesSendMessageRequest{
			Peer:        peer,
			Message:     notificationMsg,
			ReplyMarkup: markup,
		}
		_, err = b.tgCtx.SendMessage(admin.ChatID, req)
		if err != nil {
			b.logger.Printf("Failed to notify admin %d: %v", admin.UserID, err)
		}
	}
}

func (b *TelegramBot) handleAuthorizeUser(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("📥 Received /authorize command from user ID: %d", u.EffectiveUser().ID)

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
		peer := b.tgCtx.PeerStorage.GetInputPeerById(targetUserInfo.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: fmt.Sprintf("You have been authorized%s to use WebBridgeBot!", adminMsgSuffix),
		}
		_, err = b.tgCtx.SendMessage(targetUserInfo.ChatID, req)
		if err != nil {
			b.logger.Printf("Could not send notification to authorized user %d: %v", targetUserID, err)
		}
	} else {
		b.logger.Printf("Could not get user info for user %d: %v", targetUserID, err)
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been authorized%s.", targetUserID, adminMsgSuffix))
}

func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("📥 Received /deauthorize command from user ID: %d", u.EffectiveUser().ID)

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
		peer := b.tgCtx.PeerStorage.GetInputPeerById(targetUserInfo.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: "You have been deauthorized from using WebBridgeBot.",
		}
		_, err = b.tgCtx.SendMessage(targetUserInfo.ChatID, req)
		if err != nil {
			b.logger.Printf("Could not send notification to deauthorized user %d: %v", targetUserID, err)
		}
	} else {
		b.logger.Printf("Could not get user info for user %d: %v", targetUserID, err)
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been deauthorized.", targetUserID))
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	// This handler logs all incoming updates for monitoring and debugging
	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Received update from user")

		if u.EffectiveMessage != nil {
			user := u.EffectiveUser()
			chatID := u.EffectiveChat().GetID()
			message := u.EffectiveMessage

			// Log basic message info
			b.logger.Printf("[DEBUG] Message from user: %s %s (ID: %d, Username: @%s) in chat: %d",
				user.FirstName, user.LastName, user.ID, user.Username, chatID)

			// Log message ID and date
			b.logger.Printf("[DEBUG] Message ID: %d, Date: %d", message.Message.ID, message.Message.Date)

			// Check if forwarded
			if fwdFrom, isForwarded := message.Message.GetFwdFrom(); isForwarded {
				b.logger.Printf("[DEBUG] ⏩ FORWARDED message - Original date: %d, FromID: %v, FromName: %s",
					fwdFrom.Date, fwdFrom.FromID, fwdFrom.FromName)
			}

			// Log text content if present
			if message.Text != "" {
				// Truncate long messages for logging
				textPreview := message.Text
				if len(textPreview) > 100 {
					textPreview = textPreview[:100] + "..."
				}
				b.logger.Printf("[DEBUG] 💬 Text message: \"%s\"", textPreview)
			}

			// Log media type if present
			if message.Message.Media != nil {
				mediaType := fmt.Sprintf("%T", message.Message.Media)
				b.logger.Printf("[DEBUG] 📎 Media attached - Type: %s", mediaType)

				// Log specific media details
				switch media := message.Message.Media.(type) {
				case *tg.MessageMediaDocument:
					if doc, ok := media.Document.AsNotEmpty(); ok {
						b.logger.Printf("[DEBUG]    Document ID: %d, Size: %d bytes, MimeType: %s",
							doc.ID, doc.Size, doc.MimeType)
					}
				case *tg.MessageMediaPhoto:
					if photo, ok := media.Photo.AsNotEmpty(); ok {
						b.logger.Printf("[DEBUG]    Photo ID: %d, HasStickers: %t",
							photo.ID, photo.HasStickers)
					}
				}
			}

			// Log reply information if present
			if replyTo, ok := message.Message.GetReplyTo(); ok {
				if replyMsg, ok := replyTo.(*tg.MessageReplyHeader); ok {
					b.logger.Printf("[DEBUG] 💬 Reply to message ID: %d", replyMsg.ReplyToMsgID)
				}
			}

			// Log if message has buttons/markup
			if markup, ok := message.Message.GetReplyMarkup(); ok {
				b.logger.Printf("[DEBUG] 🔘 Message has reply markup: %T", markup)
			}
		}

		// Log callback queries
		if u.CallbackQuery != nil {
			b.logger.Printf("[DEBUG] 🔘 Callback query from user %d: %s",
				u.CallbackQuery.UserID, string(u.CallbackQuery.Data))
		}
	}

	return nil
}

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID() // This will correctly be the forwarding user's ID in a private chat
	user := u.EffectiveUser()           // This might be the original sender's ID for forwarded messages

	// Check if this is a forwarded message
	fwdHeader, isForwarded := u.EffectiveMessage.Message.GetFwdFrom()
	messageType := "direct upload"
	if isForwarded {
		messageType = "forwarded message"
		// Debug: Log forwarded message details
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Forwarded message detected - Date: %d, FromID: %v, FromName: %s",
				fwdHeader.Date,
				fwdHeader.FromID,
				fwdHeader.FromName)
		}
	}

	// Main log entry for media messages
	b.logger.Printf("📥 Received media %s from user: %s (ID: %d) in chat: %d", messageType, user.FirstName, user.ID, chatID)

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Message ID: %d, Media Type: %T", u.EffectiveMessage.Message.ID, u.EffectiveMessage.Message.Media)
	}

	if !b.isUserChat(ctx, chatID) {
		return dispatcher.EndGroups // Only process media from private chats
	}

	existingUser, err := b.userRepository.GetUserInfo(chatID)
	if err != nil {
		if err == sql.ErrNoRows { // User not in DB
			b.logger.Printf("User %d not in DB for media message, sending unauthorized message.", chatID)
			if b.config.DebugMode {
				b.logger.Printf("[DEBUG] User %s (ID: %d) not found in database. Message type: %s", user.FirstName, chatID, messageType)
			}
			authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
			return b.sendReply(ctx, u, authorizationMsg)
		}
		b.logger.Printf("Failed to retrieve user info from DB for media message for user %d: %v", chatID, err)
		return fmt.Errorf("failed to retrieve user info for media handling: %w", err)
	}

	b.logger.Printf("User %d retrieved for media message. isAuthorized=%t, isAdmin=%t", chatID, existingUser.IsAuthorized, existingUser.IsAdmin)

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] User details - Name: %s %s, Username: %s, ChatID: %d",
			existingUser.FirstName, existingUser.LastName, existingUser.Username, existingUser.ChatID)
	}

	if !existingUser.IsAuthorized {
		b.logger.Printf("DEBUG: User %d is NOT authorized (isAuthorized=%t). Sending unauthorized message for media.", chatID, existingUser.IsAuthorized)
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}

	// If a log channel is configured, forward the message there.
	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Log channel configured: %s. Starting message forwarding in background.", b.config.LogChannelID)
		}
		go func() { // Run in a goroutine to not block the user response.
			fromChatID := u.EffectiveChat().GetID()
			messageID := u.EffectiveMessage.Message.ID

			if b.config.DebugMode {
				b.logger.Printf("[DEBUG] Forwarding message %d from chat %d to log channel %s", messageID, fromChatID, b.config.LogChannelID)
			}

			updates, err := utils.ForwardMessages(ctx, fromChatID, b.config.LogChannelID, messageID)
			if err != nil {
				b.logger.Printf("Failed to forward message %d from chat %d to log channel %s: %v", messageID, fromChatID, b.config.LogChannelID, err)
				return // Can't proceed if forwarding failed.
			}

			b.logger.Printf("Successfully forwarded message %d from chat %d to log channel %s", messageID, fromChatID, b.config.LogChannelID)

			// Find the ID of the message just forwarded to the log channel
			var newMsgID int
			for _, update := range updates.GetUpdates() {
				if newMsg, ok := update.(*tg.UpdateNewChannelMessage); ok {
					if m, ok := newMsg.Message.(*tg.Message); ok {
						newMsgID = m.GetID()
						break
					}
				}
			}

			if newMsgID == 0 {
				b.logger.Printf("Could not find new message ID in forward-updates for original msg %d", messageID)
				return // Cannot send reply without the ID
			}

			// Get user info for the follow-up message.
			userInfo, err := b.userRepository.GetUserInfo(fromChatID)
			if err != nil {
				b.logger.Printf("Could not get user info for user %d to send to log channel", fromChatID)
				return
			}

			// Construct the informational message
			var usernameDisplay string
			if userInfo.Username != "" {
				usernameDisplay = "@" + userInfo.Username // This will create a clickable mention.
			} else {
				usernameDisplay = "N/A"
			}

			infoMsg := fmt.Sprintf("Media from user:\nID: %d\nName: %s %s\nUsername: %s",
				userInfo.UserID,
				userInfo.FirstName,
				userInfo.LastName,
				usernameDisplay,
			)

			// Get the peer for the log channel to send the reply.
			logChannelPeer, err := utils.GetLogChannelPeer(ctx, b.config.LogChannelID)
			if err != nil {
				b.logger.Printf("Failed to get log channel peer %s to send reply: %v", b.config.LogChannelID, err)
				return
			}

			// Send the informational message as a reply to the forwarded media using the raw API.
			_, err = ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     logChannelPeer,
				Message:  infoMsg,
				ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: newMsgID},
				RandomID: rand.Int63(),
			})
			if err != nil {
				b.logger.Printf("Failed to send user info to log channel %s as reply: %v", b.config.LogChannelID, err)
			}
		}()
	}

	// Check if media type is supported and extract DocumentFile
	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Attempting to extract file information from media for message ID %d", u.EffectiveMessage.Message.ID)
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		b.logger.Printf("Error processing media message from chat ID %d, message ID %d: %v", chatID, u.EffectiveMessage.Message.ID, err)
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Failed to extract file from media type: %T, error: %v", u.EffectiveMessage.Message.Media, err)
		}
		return b.sendReply(ctx, u, fmt.Sprintf("Unsupported media type or error processing file: %v", err))
	}

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] File extracted successfully - Name: %s, Size: %d bytes, MimeType: %s, ID: %d",
			file.FileName, file.FileSize, file.MimeType, file.ID)
		if file.Width > 0 || file.Height > 0 {
			b.logger.Printf("[DEBUG] Video/Photo dimensions: %dx%d", file.Width, file.Height)
		}
		if file.Duration > 0 {
			b.logger.Printf("[DEBUG] Media duration: %d seconds", file.Duration)
		}
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	b.logger.Printf("Generated media file URL for message ID %d in chat ID %d: %s (forwarded: %t)", u.EffectiveMessage.Message.ID, chatID, fileURL, isForwarded)

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Sending media to user. Message type: %s, FileURL length: %d", messageType, len(fileURL))
	}

	return b.sendMediaToUser(ctx, u, fileURL, file, isForwarded)
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
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Failed to send reply to user: %s (ID: %d) - Error: %v", u.EffectiveUser().FirstName, u.EffectiveUser().ID, err)
	}
	return err
}

func (b *TelegramBot) sendMediaURLReply(ctx *ext.Context, u *ext.Update, msg, webURL string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{
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

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, isForwarded bool) error {
	// Customize message based on whether it's forwarded or not
	messageText := fileURL
	if isForwarded {
		messageText = fmt.Sprintf("%s", fileURL)
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Using forwarded media message template for chat ID %d", u.EffectiveChat().GetID())
		}
	} else {
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Using direct upload message template for chat ID %d", u.EffectiveChat().GetID())
		}
	}

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Sending reply with inline keyboard to user %d. Message text length: %d",
			u.EffectiveUser().ID, len(messageText))
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(messageText), &ext.ReplyOpts{
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
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Failed to send media message reply: %v", err)
		}
		return err
	}

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Reply sent successfully. Preparing WebSocket message for chat ID %d", u.EffectiveChat().GetID())
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] WebSocket message constructed with %d fields. Publishing to chat ID %d", len(wsMsg), u.EffectiveChat().GetID())
	}

	b.publishToWebSocket(u.EffectiveChat().GetID(), wsMsg)

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Media processing completed successfully for message ID %d", u.EffectiveMessage.Message.ID)
	}

	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	return map[string]string{
		"url":         fileURL,
		"fileName":    file.FileName,
		"fileId":      strconv.FormatInt(file.ID, 10), // Use FormatInt for int64
		"mimeType":    file.MimeType,
		"duration":    strconv.Itoa(file.Duration),
		"width":       strconv.Itoa(file.Width),
		"height":      strconv.Itoa(file.Height),
		"title":       file.Title,                           // Audio title
		"performer":   file.Performer,                       // Audio artist/performer
		"isVoice":     strconv.FormatBool(file.IsVoice),     // Voice message flag
		"isAnimation": strconv.FormatBool(file.IsAnimation), // Animation/GIF flag
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
			peer := b.tgCtx.PeerStorage.GetInputPeerById(targetUserInfo.ChatID)
			req := &tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: userNotificationMessage,
			}
			_, err = b.tgCtx.SendMessage(targetUserInfo.ChatID, req)
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
	router.HandleFunc("/avatar/{chatID}", b.handleAvatar)
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
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] WebSocket: Invalid chat ID in request from %s", r.RemoteAddr)
		}
		return
	}

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] WebSocket connection attempt from %s for chat ID %d", r.RemoteAddr, chatID)
	}

	// Authorize user based on chatID (assuming chatID from URL is the user's ID in private chat)
	userInfo, err := b.userRepository.GetUserInfo(chatID)
	if err != nil || !userInfo.IsAuthorized {
		http.Error(w, "Unauthorized WebSocket connection: User not found or not authorized.", http.StatusUnauthorized)
		b.logger.Printf("Unauthorized WebSocket connection attempt for chatID %d: User not found or not authorized (%v)", chatID, err) // Added detailed log
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] WebSocket: Authorization failed for chat ID %d from %s", chatID, r.RemoteAddr)
		}
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

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Stream request details - MessageID: %s, Hash: %s, Range: %s, User-Agent: %s",
			messageIDStr, authHash, r.Header.Get("Range"), r.Header.Get("User-Agent"))
	}

	// Parse and validate message ID.
	messageID, err := strconv.Atoi(messageIDStr)
	if err != nil {
		b.logger.Printf("Invalid message ID '%s' received from client %s", messageIDStr, r.RemoteAddr)
		http.Error(w, "Invalid message ID format", http.StatusBadRequest)
		return
	}

	// Fetch the file information from Telegram (or cache)
	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Fetching file information for message ID %d", messageID)
	}

	file, err := utils.FileFromMessage(ctx, b.tgClient, messageID)
	if err != nil {
		b.logger.Printf("Error fetching file for message ID %d: %v", messageID, err)
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] File fetch failed for message ID %d: %v", messageID, err)
		}
		http.Error(w, "Unable to retrieve file for the specified message", http.StatusBadRequest)
		return
	}

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] File retrieved: %s (%d bytes)", file.FileName, file.FileSize)
	}

	// Hash verification
	expectedHash := utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID)
	if !utils.CheckHash(authHash, expectedHash, b.config.HashLength) {
		b.logger.Printf("Hash verification failed for message ID %d from client %s", messageID, r.RemoteAddr)
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Hash mismatch - Expected: %s..., Got: %s", expectedHash[:10], authHash)
		}
		http.Error(w, "Invalid authentication hash", http.StatusBadRequest)
		return
	}

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Hash verification passed for message ID %d", messageID)
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
	lr, err := reader.NewTelegramReader(context.Background(), b.tgClient, file.Location, start, end, contentLength, b.config.BinaryCache, b.logger)
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
		// These errors are expected if the client disconnects (e.g., closes tab, seeks video).
		// We log them differently to reduce noise from non-critical errors.
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
			b.logger.Printf("Client disconnected during stream for message ID %d. Error: %v", messageID, err)
		} else {
			b.logger.Printf("Error streaming content for message ID %d: %v", messageID, err)
		}
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

// handleAvatar serves a user's Telegram profile photo (small square) if available.
// It authorizes against known users and streams a cached small thumbnail.
func (b *TelegramBot) handleAvatar(w http.ResponseWriter, r *http.Request) {
	chatID, err := b.parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Authorize user based on chatID
	userInfo, err := b.userRepository.GetUserInfo(chatID)
	if err != nil || !userInfo.IsAuthorized {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	// Resolve InputUser from peer storage to query photos
	peer := b.tgCtx.PeerStorage.GetInputPeerById(chatID)

	var inputUser tg.InputUserClass
	switch p := peer.(type) {
	case *tg.InputPeerUser:
		inputUser = &tg.InputUser{UserID: p.UserID, AccessHash: p.AccessHash}
	case *tg.InputPeerSelf:
		inputUser = &tg.InputUserSelf{}
	default:
		http.Error(w, "User peer not found", http.StatusNotFound)
		return
	}

	// Fetch latest user photos (limit 1)
	photosRes, err := b.tgClient.API().PhotosGetUserPhotos(ctx, &tg.PhotosGetUserPhotosRequest{
		UserID: inputUser,
		Offset: 0,
		MaxID:  0,
		Limit:  1,
	})
	if err != nil {
		b.logger.Printf("Avatar: failed PhotosGetUserPhotos for %d: %v", chatID, err)
		http.NotFound(w, r)
		return
	}

	var photo *tg.Photo
	switch pr := photosRes.(type) {
	case *tg.PhotosPhotos:
		if len(pr.Photos) == 0 {
			http.NotFound(w, r)
			return
		}
		if p, ok := pr.Photos[0].(*tg.Photo); ok {
			photo = p
		}
	case *tg.PhotosPhotosSlice:
		if len(pr.Photos) == 0 {
			http.NotFound(w, r)
			return
		}
		if p, ok := pr.Photos[0].(*tg.Photo); ok {
			photo = p
		}
	default:
		http.NotFound(w, r)
		return
	}

	if photo == nil || photo.AccessHash == 0 {
		http.NotFound(w, r)
		return
	}

	// Choose a reasonable thumbnail size type (prefer "x" then fallback)
	thumbType := "x"
	var sizeBytes int
	for _, s := range photo.Sizes {
		if ps, ok := s.(*tg.PhotoSize); ok {
			if ps.Type == "x" {
				thumbType = ps.Type
				sizeBytes = ps.Size
				break
			}
			// Track fallback if no "x" exists
			if sizeBytes == 0 && ps.Size > 0 {
				thumbType = ps.Type
				sizeBytes = ps.Size
			}
		}
	}
	if sizeBytes <= 0 {
		// Fallback size when size unknown; stream a small reasonable amount via reader with a large end
		sizeBytes = 256 * 1024 // 256 KiB heuristic for small thumbnail
	}

	// Build location for the chosen thumbnail
	location := &tg.InputPhotoFileLocation{
		ID:            photo.ID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		ThumbSize:     thumbType,
	}

	// Stream using existing Telegram reader (it also caches chunks)
	start := int64(0)
	end := int64(sizeBytes - 1)
	if end < 0 {
		end = 0
	}

	rc, err := reader.NewTelegramReader(ctx, b.tgClient, location, start, end, int64(sizeBytes), b.config.BinaryCache, b.logger)
	if err != nil {
		b.logger.Printf("Avatar: reader init failed for %d: %v", chatID, err)
		http.NotFound(w, r)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// Best-effort content-length
	if sizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(sizeBytes))
	}

	if _, err := io.Copy(w, rc); err != nil {
		// Client disconnects are common; respond gracefully
		b.logger.Printf("Avatar: stream error for %d: %v", chatID, err)
	}
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
	b.logger.Printf("📥 Received /listusers command from user ID: %d", u.EffectiveUser().ID)

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
	msg.WriteString("👥 User List\n\n")
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
		msg.WriteString(fmt.Sprintf("%d. ID:%d %s %s (@%s) - Auth: %s Admin: %s\n",
			offset+i+1, user.UserID, user.FirstName, user.LastName, username, status, adminStatus))
	}

	totalPages := (totalUsers + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, totalUsers))

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

	_, err = ctx.Reply(u, ext.ReplyTextString(msg.String()), &ext.ReplyOpts{
		Markup: markup,
	})
	return err
}

// handleUserInfo retrieves detailed information about a specific user.
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("📥 Received /userinfo command from user ID: %d", u.EffectiveUser().ID)

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

	msg := fmt.Sprintf(
		"👤 User Details:\n"+
			"ID: %d\n"+
			"Chat ID: %d\n"+
			"First Name: %s\n"+
			"Last Name: %s\n"+
			"Username: @%s\n"+
			"Status: %s\n"+
			"Admin: %s\n"+
			"Joined: %s",
		targetUserInfo.UserID,
		targetUserInfo.ChatID,
		targetUserInfo.FirstName,
		targetUserInfo.LastName,
		username,
		status,
		adminStatus,
		targetUserInfo.CreatedAt,
	)

	_, err = ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

// escapeMarkdownV2 escapes characters that have special meaning in Telegram MarkdownV2.
func escapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)",
		"~", "\\~", "`", "\\`", ">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}", ".", "\\.", "!", "\\!",
	)
	return replacer.Replace(text)
}
