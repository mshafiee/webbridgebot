package bot

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"
	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/storage"
	"github.com/glebarez/sqlite"
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
)

// TelegramBot represents the main bot structure.
type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

// NewTelegramBot creates a new instance of TelegramBot.
func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
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

	tgCtx := tgClient.CreateContext()

	// Create web server
	webServer := web.NewServer(config, tgClient, tgCtx, log, userRepository)

	return &TelegramBot{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepository,
		db:             db,
		webServer:      webServer,
	}, nil
}

// Run starts the Telegram bot and web server.
func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)

	b.registerHandlers()

	go b.webServer.Start()

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

		// Log message text and entities
		if u.EffectiveMessage.Message.Message != "" {
			b.logger.Printf("[DEBUG] Message text length: %d", len(u.EffectiveMessage.Message.Message))
		}
		if len(u.EffectiveMessage.Message.Entities) > 0 {
			b.logger.Printf("[DEBUG] Message has %d entities:", len(u.EffectiveMessage.Message.Entities))
			for i, entity := range u.EffectiveMessage.Message.Entities {
				b.logger.Printf("[DEBUG]   Entity %d: Type=%T, Offset=%d, Length=%d",
					i, entity, entity.GetOffset(), entity.GetLength())
				if urlEntity, ok := entity.(*tg.MessageEntityTextURL); ok {
					b.logger.Printf("[DEBUG]     URL: %s", urlEntity.URL)
				}
			}
		}

		// Log media structure
		if webPageMedia, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			b.logger.Printf("[DEBUG] MessageMediaWebPage detected - Webpage type: %T", webPageMedia.Webpage)
		}
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
		// If FileFromMedia fails, try to extract URL from message entities (fallback for WebPageEmpty)
		if webPageMedia, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			if _, isEmpty := webPageMedia.Webpage.(*tg.WebPageEmpty); isEmpty {
				// Try to extract URL from message entities
				fileURL := utils.ExtractURLFromEntities(u.EffectiveMessage.Message)
				if fileURL != "" {
					if b.config.DebugMode {
						b.logger.Printf("[DEBUG] Extracted URL from message entities: %s", fileURL)
					}

					// Quick check if URL might be a file hosting page
					isFileHosting := strings.Contains(strings.ToLower(fileURL), "attach.fahares.com") ||
						strings.Contains(strings.ToLower(fileURL), "filehosting") ||
						strings.Contains(strings.ToLower(fileURL), "upload")

					// Detect MIME type from URL
					mimeType := utils.DetectMimeTypeFromURL(fileURL)
					if b.config.DebugMode {
						b.logger.Printf("[DEBUG] Detected MIME type from URL: %s", mimeType)
						if isFileHosting {
							b.logger.Printf("[DEBUG] Warning: URL appears to be a file hosting page")
						}
					}

					// Create a simple DocumentFile for URL-based media
					file = &types.DocumentFile{
						FileName: "external_media",
						MimeType: mimeType,
						FileSize: 0, // Unknown size
					}

					// Send the URL to the user (with potential warning)
					err := b.sendMediaToUser(ctx, u, fileURL, file, isForwarded)

					// If it's a file hosting URL, send an additional warning
					if err == nil && isFileHosting {
						warningMsg := "⚠️ Note: This appears to be a file hosting page. If the media doesn't play, please:\n" +
							"• Send the file directly (not forwarded)\n" +
							"• Or provide a direct download link"
						time.Sleep(500 * time.Millisecond) // Small delay before sending warning
						_ = b.sendReply(ctx, u, warningMsg)
					}

					return err
				}
			}
		}

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

	// Build keyboard rows
	var keyboardRows []tg.KeyboardButtonRow

	// First row: Resend button and optionally Stream URL button
	// Note: Telegram doesn't accept localhost URLs in inline buttons, so we skip it for localhost
	firstRowButtons := []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{
			Text: "Resend to Player",
			Data: []byte(fmt.Sprintf("%s,%d", callbackResendToPlayer, u.EffectiveMessage.Message.ID)),
		},
	}

	// Only add URL button if not using localhost (Telegram doesn't allow localhost in URL buttons)
	if !strings.Contains(strings.ToLower(fileURL), "localhost") &&
		!strings.Contains(strings.ToLower(fileURL), "127.0.0.1") {
		firstRowButtons = append(firstRowButtons, &tg.KeyboardButtonURL{Text: "Stream URL", URL: fileURL})
	}

	keyboardRows = append(keyboardRows, tg.KeyboardButtonRow{Buttons: firstRowButtons})

	// Second row: Fullscreen toggle
	keyboardRows = append(keyboardRows, tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "Toggle Fullscreen", Data: []byte(callbackToggleFullscreen)},
		},
	})

	// Third row: Playback controls
	keyboardRows = append(keyboardRows, tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "▶️/⏸️", Data: []byte(callbackPlay)},
			&tg.KeyboardButtonCallback{Text: "🔄", Data: []byte(callbackRestart)},
			&tg.KeyboardButtonCallback{Text: "⏪ 10s", Data: []byte(callbackBackward10)},
			&tg.KeyboardButtonCallback{Text: "⏩ 10s", Data: []byte(callbackForward10)},
		},
	})

	_, err := ctx.Reply(u, ext.ReplyTextString(messageText), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{
			Rows: keyboardRows,
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

	b.webServer.GetWSManager().PublishMessage(u.EffectiveChat().GetID(), wsMsg)

	if b.config.DebugMode {
		b.logger.Printf("[DEBUG] Media processing completed successfully for message ID %d", u.EffectiveMessage.Message.ID)
	}

	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	// Wrap external URLs with proxy to avoid CORS issues
	proxiedURL := b.wrapWithProxyIfNeeded(fileURL)

	if b.config.DebugMode && proxiedURL != fileURL {
		b.logger.Printf("[DEBUG] Wrapped external URL with proxy: %s -> %s", fileURL, proxiedURL)
	}

	return map[string]string{
		"url":         proxiedURL,
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

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(
		file.FileName,
		file.FileSize,
		file.MimeType,
		file.ID,
	), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
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
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Callback: Processing ResendToPlayer, data: %s", string(u.CallbackQuery.Data))
		}
		dataParts := strings.Split(string(u.CallbackQuery.Data), ",")
		if b.config.DebugMode {
			b.logger.Printf("[DEBUG] Callback: Split into %d parts: %v", len(dataParts), dataParts)
		}
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
			var fileURL string

			if err != nil {
				// Fallback: Try to extract URL from message entities (for WebPageEmpty)
				message, msgErr := utils.GetMessage(ctx, b.tgClient, messageID)
				if msgErr == nil && message.Media != nil {
					if webPageMedia, ok := message.Media.(*tg.MessageMediaWebPage); ok {
						if _, isEmpty := webPageMedia.Webpage.(*tg.WebPageEmpty); isEmpty {
							extractedURL := utils.ExtractURLFromEntities(message)
							if extractedURL != "" {
								if b.config.DebugMode {
									b.logger.Printf("[DEBUG] Callback: Extracted URL from entities for message %d: %s", messageID, extractedURL)
								}
								// Detect MIME type from URL
								mimeType := utils.DetectMimeTypeFromURL(extractedURL)
								if b.config.DebugMode {
									b.logger.Printf("[DEBUG] Callback: Detected MIME type: %s", mimeType)
								}
								// Create a simple DocumentFile for URL-based media
								file = &types.DocumentFile{
									FileName: "external_media",
									MimeType: mimeType,
									FileSize: 0,
								}
								fileURL = extractedURL
								if b.config.DebugMode {
									b.logger.Printf("[DEBUG] Callback: Set fileURL to extracted URL, length: %d", len(fileURL))
								}
							}
						}
					}
				}

				if b.config.DebugMode {
					b.logger.Printf("[DEBUG] Callback: After fallback, fileURL length: %d, file is nil: %v", len(fileURL), file == nil)
				}

				if fileURL == "" {
					b.logger.Printf("Error fetching file for message ID %d for callback: %v", messageID, err)
					_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
						QueryID: u.CallbackQuery.QueryID,
						Message: "Failed to retrieve file info.",
					})
					return nil
				}
			} else {
				fileURL = b.generateFileURL(messageID, file)
				if b.config.DebugMode {
					b.logger.Printf("[DEBUG] Callback: Generated Telegram file URL")
				}
			}

			if b.config.DebugMode {
				b.logger.Printf("[DEBUG] Callback: Constructing WebSocket message with URL: %s, MIME: %s", fileURL, file.MimeType)
			}

			wsMsg := b.constructWebSocketMessage(fileURL, file)
			b.webServer.GetWSManager().PublishMessage(u.EffectiveChat().GetID(), wsMsg)

			if b.config.DebugMode {
				b.logger.Printf("[DEBUG] Callback: WebSocket message published successfully")
			}

			successMsg := fmt.Sprintf("The %s file has been sent to the web player.", file.FileName)
			if b.config.DebugMode {
				b.logger.Printf("[DEBUG] Callback: Sending success answer to Telegram: %s", successMsg)
			}

			_, err = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				Alert:   false,
				QueryID: u.CallbackQuery.QueryID,
				Message: successMsg,
			})
			if err != nil && b.config.DebugMode {
				b.logger.Printf("[DEBUG] Callback: Error sending answer to Telegram: %v", err)
			} else if b.config.DebugMode {
				b.logger.Printf("[DEBUG] Callback: Success answer sent to Telegram successfully")
			}
			return nil
		} else {
			// Handle case where callback data format is incorrect
			if b.config.DebugMode {
				b.logger.Printf("[DEBUG] Callback: Invalid data format for ResendToPlayer, parts: %d", len(dataParts))
			}
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "Invalid callback data format.",
			})
			return nil
		}
	}

	// For other simple commands, they are simple string matches.
	callbackType := string(u.CallbackQuery.Data)
	chatID := u.EffectiveChat().GetID() // The chat ID to which the web player is linked.

	switch callbackType {
	case callbackPlay:
		if _, ok := b.webServer.GetWSManager().GetClient(chatID); ok {
			b.webServer.GetWSManager().PublishControlCommand(chatID, "togglePlayPause", nil)
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
		if _, ok := b.webServer.GetWSManager().GetClient(chatID); ok {
			b.webServer.GetWSManager().PublishControlCommand(chatID, "restart", nil)
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
		if _, ok := b.webServer.GetWSManager().GetClient(chatID); ok {
			b.webServer.GetWSManager().PublishControlCommand(chatID, "seek", 10)
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
		if _, ok := b.webServer.GetWSManager().GetClient(chatID); ok {
			b.webServer.GetWSManager().PublishControlCommand(chatID, "seek", -10)
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
	case callbackToggleFullscreen:
		if _, ok := b.webServer.GetWSManager().GetClient(chatID); ok {
			b.webServer.GetWSManager().PublishControlCommand(chatID, "toggleFullscreen", nil)
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

// wrapWithProxyIfNeeded wraps external URLs with the proxy endpoint
func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	// Check if it's an external URL (http:// or https://)
	if strings.HasPrefix(fileURL, "http://") || strings.HasPrefix(fileURL, "https://") {
		// Check if it's NOT already our own server
		if !strings.Contains(fileURL, fmt.Sprintf(":%s", b.config.Port)) &&
			!strings.Contains(fileURL, "localhost") &&
			!strings.HasPrefix(fileURL, b.config.BaseURL) {
			// Wrap with proxy
			return fmt.Sprintf("/proxy?url=%s", url.QueryEscape(fileURL))
		}
	}
	// Return as-is for local/relative URLs
	return fileURL
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
