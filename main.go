package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/zelenin/go-tdlib/client"
)

type Config struct {
	ApiID                int
	ApiHash              string
	BotToken             string
	BaseURL              string
	Port                 string
	MaxFilesFolderSizeGB int64
	TdlibParameters      *client.SetTdlibParametersRequest
}

type TelegramBot struct {
	config      *Config
	tdlibClient *client.Client
	urlHistory  map[int64]FileIdMeta
}

type FileIdMeta map[int32]FileMeta

type FileMeta struct {
	URL                    string
	MIMEType               string
	IsDownloadingCompleted bool
	Size                   int64
	Duration               int32
	Width                  int32
	Height                 int32
	FileName               string
}

var wsClients = make(map[int64]*websocket.Conn) // chatID to WebSocket connection

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func main() {
	config := loadConfig()
	initializeAndRunBot(config)
}

func loadConfig() Config {
	var (
		apiID           = flag.Int("apiID", 0, "Telegram API ID")
		apiHash         = flag.String("apiHash", "", "Telegram API Hash")
		botToken        = flag.String("botToken", "", "Telegram Bot Token")
		baseURL         = flag.String("baseURL", "", "Base URL for the webhook")
		port            = flag.String("port", "8080", "Port on which the bot runs")
		useIPAsBaseURL  = flag.Bool("local", false, "Use the machine's IP address as the base URL")
		maxFolderSizeGB = flag.Int64("maxFolderSizeGB", 10, "Maximum size of the download folder in gigabytes")
	)
	flag.Parse()

	if *apiID == 0 {
		log.Fatal("apiID flag is required and not set")
	}
	if *apiHash == "" {
		log.Fatal("apiHash flag is required and not set")
	}
	if *botToken == "" {
		log.Fatal("botToken flag is required and not set")
	}
	if *baseURL == "" {
		log.Fatal("baseURL flag is required and not set")
	}

	if *useIPAsBaseURL {
		ips, err := findIPAddresses()
		if err != nil {
			fmt.Println("Error finding IP addresses:", err)
			os.Exit(1)
		}
		if len(ips) > 0 {
			*baseURL = "http://" + ips[0] + ":" + *port
		} else {
			fmt.Println("No valid IP address found. Using default base URL.")
		}
	}

	return Config{
		ApiID:                *apiID,
		ApiHash:              *apiHash,
		BotToken:             *botToken,
		BaseURL:              *baseURL,
		Port:                 *port,
		MaxFilesFolderSizeGB: *maxFolderSizeGB,
		TdlibParameters: &client.SetTdlibParametersRequest{
			UseTestDc:              false,
			DatabaseDirectory:      filepath.Join(".tdlib", "database"),
			FilesDirectory:         filepath.Join(".tdlib", "files"),
			UseFileDatabase:        true,
			UseChatInfoDatabase:    true,
			UseMessageDatabase:     true,
			UseSecretChats:         false,
			ApiId:                  int32(*apiID),
			ApiHash:                *apiHash,
			SystemLanguageCode:     "en",
			DeviceModel:            "Server",
			SystemVersion:          "1.0.0",
			ApplicationVersion:     "1.0.0",
			EnableStorageOptimizer: true,
			IgnoreFileNames:        false,
		},
	}
}

func initializeAndRunBot(config Config) {
	bot := &TelegramBot{
		config:     &config,
		urlHistory: make(map[int64]FileIdMeta),
	}
	bot.Run()
}

func (b *TelegramBot) Run() {
	// Initialize the TDLib client
	authorizer := client.BotAuthorizer(b.config.BotToken)
	authorizer.TdlibParameters <- b.config.TdlibParameters
	b.tdlibClient = b.initTDLibClient(authorizer)

	// Get the bot's information
	me := b.getMe()
	log.Printf("Authorized as bot: %s", strings.Join(me.Usernames.ActiveUsernames, ", "))

	// Start the web server
	go b.startWebServer()

	// Start processing updates
	listener := b.tdlibClient.GetListener()
	defer listener.Close()
	b.processUpdates(listener)
}

func (b *TelegramBot) initTDLibClient(authorizer client.AuthorizationStateHandler) *client.Client {
	// Set the log verbosity level
	_, err := client.SetLogVerbosityLevel(&client.SetLogVerbosityLevelRequest{
		NewVerbosityLevel: 1,
	})
	if err != nil {
		log.Fatalf("SetLogVerbosityLevel error: %s", err)
	}

	// Create a new TDLib client
	tdlibClient, err := client.NewClient(authorizer)
	if err != nil {
		log.Fatalf("NewClient error: %s", err)
	}

	// Get the TDLib version
	optionValue, err := client.GetOption(&client.GetOptionRequest{
		Name: "version",
	})
	if err != nil {
		log.Fatalf("GetOption error: %s", err)
	}
	log.Printf("TDLib version: %s", optionValue.(*client.OptionValueString).Value)

	return tdlibClient
}

func (b *TelegramBot) getMe() *client.User {
	me, err := b.tdlibClient.GetMe()
	if err != nil {
		log.Fatalf("GetMe error: %s", err)
	}
	return me
}

func (b *TelegramBot) processUpdates(listener *client.Listener) {
	for update := range listener.Updates {
		switch update.GetType() {
		case client.TypeUpdateNewMessage:
			log.Printf("Received UpdateNewMessage: %#v", update)
			updateNewMessage := update.(*client.UpdateNewMessage)
			message := updateNewMessage.Message
			b.processMessage(message.ChatId, message)
		case client.TypeUpdateUser:
			log.Printf("Received UpdateUser: %#v", update)
			updateUser := update.(*client.UpdateUser)
			b.processUpdateUser(updateUser)
		case client.TypeUpdateFile:
			updateFile := update.(*client.UpdateFile)
			b.processUpdateFile(updateFile)
			break
		case client.TypeMessage:
			break
		case client.TypeUpdateNewChat:
			break
		case client.TypeUpdateMessageSendSucceeded:
			break
		case client.TypeError:
			errorMessage := update.(*client.Error)
			log.Printf("Telegram Error Message: %d, %s", errorMessage.Code, errorMessage.Message)
			break
		default:
			log.Printf("Unhandled update: %#v", update)
			PrintAllFields(update)
		}
	}
}

func (b *TelegramBot) processUpdateUser(update *client.UpdateUser) {
	var activeUsernames []string
	if update.User.Usernames != nil {
		activeUsernames = update.User.Usernames.ActiveUsernames
	}
	log.Printf("UpdateUser - UserID: %d, FirstName: %s, LastName: %s, activeUsernames: %v", update.User.Id, update.User.FirstName, update.User.LastName, activeUsernames)
	// Note: Access only the exported fields
}

func (b *TelegramBot) processMessage(chatID int64, message *client.Message) {
	switch message.Content.MessageContentType() {
	case client.TypeMessageAudio:
		audio := message.Content.(*client.MessageAudio).Audio
		log.Printf("Audio: %s", audio.Audio.Id)
		b.handleForwardedAudio(chatID, message)
	case client.TypeMessageDocument:
		document := message.Content.(*client.MessageDocument).Document
		log.Printf("Document: %s", document.Document.Id)
		b.handleForwardedDocument(chatID, message)
	case client.TypeMessageVideo:
		video := message.Content.(*client.MessageVideo).Video
		log.Printf("Video: %d", video.Video.Id)
		b.handleForwardedVideo(chatID, message)
	case client.TypeMessagePhoto:
		photo := message.Content.(*client.MessagePhoto).Photo
		bestQualityPhoto := photo.Sizes[len(photo.Sizes)-1]
		log.Printf("Photo: %d", bestQualityPhoto.Photo.Id)
		b.handleForwardedPhoto(chatID, message)
	case client.TypeMessageText:
		text := message.Content.(*client.MessageText).Text.Text
		log.Printf("Text: %s", text)
		b.handleCommand(message)

	default:
		log.Printf("Unhandled content type: %s", message.Content.MessageContentType())
		// Optionally handle unknown types
	}
}

func (b *TelegramBot) processUpdateFile(updateFile *client.UpdateFile) {
	file := updateFile.File
	localFile := file.Local
	fileId := file.Id

	// Check if the file download is completed and update the URL download status
	if localFile.IsDownloadingCompleted {
		log.Printf("Download completed for file ID %d at path %s", fileId, localFile.Path)
		b.updateURLDownloadStatus(fileId, true)
	} else {
		// If the download is not completed, you might want to update the status as well. Depending on your needs,
		// you might skip this or handle it differently.
		log.Printf("Downloading... File ID %d, downloaded %d of %d bytes.", fileId, localFile.DownloadedSize, file.Size)
		b.updateURLDownloadStatus(fileId, false)
	}
}

func (b *TelegramBot) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatIDStr, ok := vars["chatID"]
	if !ok {
		http.Error(w, "Chat ID is required", http.StatusBadRequest)
		return
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
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

	// Register client
	wsClients[chatID] = ws

	for {
		// Keep connection alive or handle control messages if necessary
		// For example, read messages to prevent the connection from closing
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			log.Println(err)
			delete(wsClients, chatID)
			break
		}
		// Echo the message back (Optional, for keeping the connection alive)
		if err := ws.WriteMessage(messageType, p); err != nil {
			log.Println(err)
			break
		}
	}
}

func (b *TelegramBot) handleForwardedAudio(chatID int64, message *client.Message) {
	audio := message.Content.(*client.MessageAudio).Audio
	fileID := audio.Audio.Id
	fileSize := audio.Audio.Size
	fileURL := b.getFileURL(chatID, fileID)

	b.storeURLInHistory(chatID, fileID, fileURL, audio.MimeType, fileSize, audio.Duration, -1, -1, audio.FileName)
	b.sendMessage(message.ChatId, fileURL)

	// Construct the message with the URL and type
	wsMsg := map[string]string{
		"fileId":   strconv.Itoa(int(fileID)),
		"url":      fileURL,
		"mimeType": audio.MimeType,
		"duration": strconv.Itoa(int(audio.Duration)),
		"fileName": audio.FileName,
	}
	b.publishOverWS(chatID, wsMsg)
}

func (b *TelegramBot) handleForwardedDocument(chatID int64, message *client.Message) {
	document := message.Content.(*client.MessageDocument).Document
	fileID := document.Document.Id
	fileSize := document.Document.Size
	fileURL := b.getFileURL(chatID, fileID)
	b.storeURLInHistory(chatID, fileID, fileURL, document.MimeType, fileSize, -1, -1, -1, document.FileName)
	b.sendMessage(message.ChatId, fileURL)
}

func (b *TelegramBot) handleForwardedVideo(chatID int64, message *client.Message) {
	videoContent := message.Content.(*client.MessageVideo)
	video := videoContent.Video
	fileID := video.Video.Id
	fileSize := video.Video.Size
	fileURL := b.getFileURL(chatID, video.Video.Id)

	// Store URL in history and send the message
	b.storeURLInHistory(chatID, fileID, fileURL, video.MimeType, fileSize, video.Duration, video.Width, video.Height, video.FileName)
	b.sendMessage(message.ChatId, fileURL)

	// Construct the message with the URL and type
	wsMsg := map[string]string{
		"fileId":   strconv.Itoa(int(video.Video.Id)),
		"url":      fileURL,
		"mimeType": video.MimeType,
		"duration": strconv.Itoa(int(video.Duration)),
		"width":    strconv.Itoa(int(video.Width)),
		"height":   strconv.Itoa(int(video.Height)),
		"fileName": video.FileName,
	}
	b.publishOverWS(chatID, wsMsg)
}

func (b *TelegramBot) handleForwardedPhoto(chatID int64, message *client.Message) {
	photo := message.Content.(*client.MessagePhoto).Photo
	bestQualityPhoto := photo.Sizes[len(photo.Sizes)-1]
	fileID := bestQualityPhoto.Photo.Id
	fileSize := bestQualityPhoto.Photo.Size
	fileURL := b.getFileURL(chatID, fileID)

	// Example MIME type determination (simplified)
	mimeType := "image/jpeg" // Default MIME type; consider a more dynamic approach as needed

	b.storeURLInHistory(chatID, fileID, fileURL, mimeType, fileSize, -1, bestQualityPhoto.Width, bestQualityPhoto.Height, "")
	b.sendMessage(message.ChatId, fileURL)

	// Construct the message with the URL and type
	wsMsg := map[string]string{
		"fileId":   strconv.Itoa(int(fileID)),
		"url":      fileURL,
		"mimeType": mimeType,
		"width":    strconv.Itoa(int(bestQualityPhoto.Width)),
		"height":   strconv.Itoa(int(bestQualityPhoto.Height)),
	}
	b.publishOverWS(chatID, wsMsg)
}

func (b *TelegramBot) publishOverWS(chatID int64, message map[string]string) {
	if client, ok := wsClients[chatID]; ok {
		// Convert the message to JSON
		messageJSON, err := json.Marshal(message)
		if err != nil {
			log.Println("Error marshalling message:", err)
			return
		}

		// Send the message over WebSocket
		if err := client.WriteMessage(websocket.TextMessage, messageJSON); err != nil {
			log.Println("Error sending message:", err)
			delete(wsClients, chatID)
			client.Close()
		}
	}
}

func (b *TelegramBot) storeURLInHistory(chatID int64, fileID int32, url string, mimeType string, size int64, duration int32, width int32, height int32, fileName string) {
	// Initialize the chatID entry in urlHistory if it doesn't exist
	if _, ok := b.urlHistory[chatID]; !ok {
		b.urlHistory[chatID] = make(FileIdMeta)
	}

	// Check if the URL is already in the history for the given chatID
	urlExists := false
	for _, fileMeta := range b.urlHistory[chatID] {
		if fileMeta.URL == url {
			urlExists = true
			break
		}
	}

	// Only add the URL, MIME type, IsDownloadingCompleted status, and fileId if the URL does not already exist
	if !urlExists {
		b.urlHistory[chatID][fileID] = FileMeta{
			URL:                    url,
			MIMEType:               mimeType,
			IsDownloadingCompleted: false,
			Size:                   size,
			Duration:               duration,
			Width:                  width,
			Height:                 height,
			FileName:               fileName,
		}
	}
}

// Update the download status of a URL associated with a fileId in the urlHistory
func (b *TelegramBot) updateURLDownloadStatus(fileId int32, isDownloadingCompleted bool) {
	// Iterate through each chat ID in the urlHistory
	for _, fileIdMeta := range b.urlHistory {
		// Check if the fileId exists in the FileIdMeta for the current chatID
		if fileMeta, ok := fileIdMeta[fileId]; ok {
			// Update the IsDownloadingCompleted status for the fileId
			fileMeta.IsDownloadingCompleted = isDownloadingCompleted
			// Since maps store references to objects, the original map is updated
			// but to follow good practice and ensure clarity, update the map explicitly
			fileIdMeta[fileId] = fileMeta
		}
	}
}

func (b *TelegramBot) getMetaByURL(chatID int64, url string) (FileMeta, error) {
	fmt.Printf("chatID: %d, url: %s, b.history: %v\n", chatID, url, b.urlHistory)
	if urlMIMEs, ok := b.urlHistory[chatID]; ok {
		for _, fileMeta := range urlMIMEs {
			if fileMeta.URL == url {
				return fileMeta, nil
			}
		}
		return FileMeta{}, fmt.Errorf("URL not found in history")
	}
	return FileMeta{}, fmt.Errorf("No history for chatID")
}

func (b *TelegramBot) handleCommand(message *client.Message) {
	chatID := message.ChatId
	webURL := fmt.Sprintf("%s/%d", b.config.BaseURL, chatID)
	var text string
	// Check if the command is '/start'
	if strings.HasPrefix(message.Content.(*client.MessageText).Text.Text, "/start") {
		text = "Welcome to WebBridgeBot, your bridge between Telegram and the Web!\n\n"
		text += "Find out more about WebBridgeBot on GitHub: https://github.com/mshafiee/webbridgebot\n\n"
		text += "Access your player and more features here:\n" + webURL + "\n\n"
	}

	if strings.HasPrefix(message.Content.(*client.MessageText).Text.Text, "/url") {
		text = "Access your player and more features here:\n" + webURL
	}

	b.sendMessage(chatID, text)
}

func (b *TelegramBot) getFileURL(chatID int64, fileID int32) string {
	return fmt.Sprintf("%s/%d/%d", b.config.BaseURL, chatID, fileID)
}

func (b *TelegramBot) sendMessage(chatID int64, text string) {
	log.Printf("Sending message to chat %d: %s", chatID, text)
	_, err := b.tdlibClient.SendMessage(&client.SendMessageRequest{
		ChatId: chatID,
		InputMessageContent: &client.InputMessageText{
			Text: &client.FormattedText{
				Text:     text,
				Entities: nil,
			},
			LinkPreviewOptions: nil,
			ClearDraft:         false,
		},
	})
	if err != nil {
		log.Printf("Error sending message: %v", err)
	}
}

func (b *TelegramBot) startWebServer() {
	router := mux.NewRouter()
	// Define the WebSocket route explicitly
	router.HandleFunc("/ws/{chatID}", b.handleWebSocket)

	// Define other routes
	router.HandleFunc("/{chatID}/{fileID}", b.handleFileDownload)
	router.HandleFunc("/{chatID}", b.handlePlayer)
	router.HandleFunc("/{chatID}/", b.handlePlayer)

	// Make sure the WebSocket route is not being caught by a more generic handler

	port := b.config.Port
	log.Printf("Web server started on port %s", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%s", port), router); err != nil {
		log.Panic(err)
	}
}
func (b *TelegramBot) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	// Extract variables from request
	chatID, fileID, err := b.extractRequestParameters(w, r)
	if err != nil {
		// Error already handled within extractRequestParameters
		return
	}

	// Download and open file
	fp, err := b.downloadAndOpenFile(fileID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer fp.Close()

	// Get file metadata
	fileSize, mimeType, err := b.getFileMetadata(fp, chatID, fileID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Handle range requests for partial content delivery
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		b.servePartialContent(w, fp, rangeHeader, fileSize, mimeType)
	} else {
		// Serve the entire file
		w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
		w.Header().Set("Content-Type", mimeType)
		io.Copy(w, fp) // Stream the whole file
	}
}

func (b *TelegramBot) extractRequestParameters(w http.ResponseWriter, r *http.Request) (chatID int64, fileID int32, err error) {
	vars := mux.Vars(r)
	chatIDStr, fileIDStr := vars["chatID"], vars["fileID"]

	chatID, err = strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return 0, 0, err
	}

	fileIDInt64, err := strconv.ParseInt(fileIDStr, 10, 32)
	if err != nil {
		http.Error(w, "Invalid file ID", http.StatusBadRequest)
		return 0, 0, err
	}

	return chatID, int32(fileIDInt64), nil
}

func (b *TelegramBot) downloadAndOpenFile(fileID int32) (*os.File, error) {
	// Attempt to clean up the download folder before downloading a new file
	if b.isCleanupNeeded() {
		go func() {
			err := b.cleanUpDownloadFolderIfNeeded()
			if err != nil {
				log.Printf("Error cleaning up download folder: %v", err)
			}
		}()
	}

	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		file, err := b.tdlibClient.DownloadFile(&client.DownloadFileRequest{
			FileId:   fileID,
			Priority: 1,
		})
		if err != nil {
			if i == maxRetries-1 {
				return nil, fmt.Errorf("Error downloading file after %d attempts: %v", maxRetries, err)
			}
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}

		fp, err := os.Open(file.Local.Path)
		if err == nil {
			return fp, nil
		}
		if i == maxRetries-1 {
			return nil, fmt.Errorf("Error opening file after %d attempts: %v", maxRetries, err)
		}
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return nil, fmt.Errorf("Unhandled error in downloadAndOpenFile")
}

func (b *TelegramBot) getFileMetadata(fp *os.File, chatID int64, fileID int32) (int64, string, error) {
	fileInfo, err := fp.Stat()
	if err != nil {
		return 0, "", fmt.Errorf("Error getting file info: %v", err)
	}

	fileMeta, err := b.getMetaByURL(chatID, b.getFileURL(chatID, fileID))
	if err != nil {
		// Default MIME type if metadata is not found
		return fileInfo.Size(), "application/octet-stream", nil
	}

	return fileMeta.Size, fileMeta.MIMEType, nil
}

func (b *TelegramBot) servePartialContent(w http.ResponseWriter, fp *os.File, rangeHeader string, fileSize int64, mimeType string) {
	start, end, err := parseRange(rangeHeader, fileSize)
	if err != nil {
		http.Error(w, "Invalid Range Header", http.StatusBadRequest)
		return
	}

	contentLength := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
	w.Header().Set("Content-Type", mimeType)
	w.WriteHeader(http.StatusPartialContent)

	fp.Seek(start, 0)
	io.CopyN(w, fp, contentLength)
}

// parseRange parses a Range header string and returns the start and end byte positions
func parseRange(rangeStr string, fileSize int64) (start, end int64, err error) {
	start = 0
	end = fileSize - 1
	rangeStr = strings.TrimPrefix(rangeStr, "bytes=")
	parts := strings.Split(rangeStr, "-")
	if parts[0] != "" {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return
		}
	}
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return
		}
	}
	if start > end || end >= fileSize {
		err = fmt.Errorf("invalid range")
	}
	return
}

func (b *TelegramBot) isCleanupNeeded() bool {
	var totalSize int64
	err := filepath.Walk(b.config.TdlibParameters.FilesDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		log.Printf("Error walking through download folder: %v", err)
		return false
	}

	// Convert maxFolderSize from GB to bytes for comparison
	maxFolderSize := b.config.MaxFilesFolderSizeGB * 1024 * 1024 * 1024
	return totalSize > maxFolderSize
}

func (b *TelegramBot) cleanUpDownloadFolderIfNeeded() error {
	var totalSize int64
	fileList := make([]struct {
		path    string
		modTime time.Time
		size    int64
	}, 0)

	// Convert maxFolderSize from GB to bytes for comparison
	maxFolderSize := b.config.MaxFilesFolderSizeGB * 1024 * 1024 * 1024

	// Walk through the download folder to calculate total size and collect file info
	err := filepath.Walk(b.config.TdlibParameters.FilesDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
			fileList = append(fileList, struct {
				path    string
				modTime time.Time
				size    int64
			}{
				path, info.ModTime(), info.Size(),
			})
		}
		return nil
	})

	if err != nil {
		return err
	}

	// Sort the files by modification time, oldest first
	sort.Slice(fileList, func(i, j int) bool {
		return fileList[i].modTime.Before(fileList[j].modTime)
	})

	// Remove files until the total size is within the limit
	for totalSize > maxFolderSize && len(fileList) > 0 {
		oldestFile := fileList[0]
		fileList = fileList[1:]
		err := os.Remove(oldestFile.path)
		if err != nil {
			return err
		}
		totalSize -= oldestFile.size
		log.Printf("Removed file %s (oldest first) to reduce download folder size", oldestFile.path)
	}

	return nil
}

func (b *TelegramBot) handlePlayer(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request for player: %s", r.URL.Path)

	vars := mux.Vars(r)
	chatIDStr, ok := vars["chatID"]
	if !ok {
		http.Error(w, "Chat ID is required", http.StatusBadRequest)
		return
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Define the HTML template with embedded JavaScript
	tmpl := `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Media Player</title>
	<style>
		body {
			margin: 0;
			padding: 10px; /* Reduced padding */
			box-sizing: border-box;
			display: flex;
			flex-direction: column;
			align-items: center;
			gap: 2px; /* Reduced gap */
			font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
			background-color: #f8f9fa;
		}
		h1 {
			color: ##a5adb6;
			font-size: 1.5rem; /* Reduced font size */
			font-weight: 600;
			margin: 0; /* Remove margin */
		}
		#videoPlayer, #audioPlayer {
			max-width: 100%;
			max-height: 50vh; /* Adjusted for compact design */
			display: none; /* Initially hidden */
			border-radius: 8px;
			box-shadow: 0 4px 6px rgba(0, 0, 0, 0.1);
		}
		#imageViewer {
			max-width: 100%; 
			max-height: 50vh; 
			display: none; 
			border-radius: 8px; 
			box-shadow: 0 4px 6px rgba(0, 0, 0, 0.1);
		}
		.button-container {
			display: flex;
			justify-content: center;
			width: 100%;
			margin: 5px 0; /* Reduced margin */
		}
		#fullscreenButton, #reloadButton {
			display: none; /* Initially hidden */
			margin: 0 5px;
			padding: 5px 15px;
			font-size: 0.9rem; /* Adjusted font size for compactness */
			font-weight: 500;
			color: #fff;
			background-color: #007bff;
			border: none;
			border-radius: 4px;
			cursor: pointer;
			transition: background-color 0.3s;
		}
		#fullscreenButton:hover, #reloadButton:hover {
			background-color: #0056b3;
		}
		#status {
			font-size: 1rem; /* Adjusted font size */
			color: #6c757d;
			margin: 5px 0; /* Adjusted margin */
		}
	</style>
</head>
<body>
    <h1>WebBridgeBot</h1>
    <p id="status">Chat ID: {{.ChatID}}; Waiting for media...</p>
    <video id="videoPlayer" controls></video>
    <audio id="audioPlayer" controls></audio>
	<img id="imageViewer" controls style="" />
    <div class="button-container">
        <button id="reloadButton">Reload</button>
        <button id="fullscreenButton">Fullscreen</button>
    </div>

    <script>
		document.addEventListener('DOMContentLoaded', () => {
			const videoPlayer = document.getElementById('videoPlayer');
			const audioPlayer = document.getElementById('audioPlayer');
    		const imageViewer = document.getElementById('imageViewer');
			const fullscreenButton = document.getElementById('fullscreenButton');
			const reloadButton = document.getElementById('reloadButton');
			const statusText = document.getElementById('status');
			let ws;
			let latestMedia = { url: null, mimeType: null };
			let attemptReconnect = true;
		
			const setupWebSocket = () => {
				const wsAddress = 'ws://' + window.location.host + '/ws/{{.ChatID}}';
				ws = new WebSocket(wsAddress);
		
				ws.addEventListener('message', (event) => handleWebSocketMessage(event));
				ws.addEventListener('error', (error) => handleWebSocketError(error));
				ws.addEventListener('open', () => handleWebSocketOpen());
				ws.addEventListener('close', () => handleWebSocketClose());
			};
		
			const handleWebSocketMessage = (event) => {
				const data = JSON.parse(event.data);
				console.log('Message from server: ', data);
				latestMedia = { url: data.url, mimeType: data.mimeType };
				playMedia(data.url, data.mimeType);
			};

            const handleWebSocketClose = () => {
                console.log('WebSocket closed. Attempting to reconnect...');
                if (attemptReconnect) setTimeout(setupWebSocket, 3000);
            };

            const handleWebSocketError = (error) => {
                console.error('WebSocket encountered an error: ', error.message);
                ws.close(); // Ensure the WebSocket is closed properly before attempting to reconnect
            };
		
			const playMedia = (url, mimeType) => {
				if (mimeType.startsWith('video')) {
					updateUIForMedia(videoPlayer, [audioPlayer, imageViewer], mimeType);
					loadAndPlayMedia(videoPlayer, url);
				} else if (mimeType.startsWith('audio')) {
					updateUIForMedia(audioPlayer, [videoPlayer, imageViewer], mimeType);
					loadAndPlayMedia(audioPlayer, url);
				} else if (mimeType.startsWith('image')) {
					updateUIForMedia(imageViewer, [videoPlayer, audioPlayer], mimeType);
					loadImage(imageViewer, url);
				} else {
					console.log('Unsupported media type: ', mimeType);
				}
			};
		
			const updateUIForMedia = (playerToShow, playersToHide, mimeType) => {
				playersToHide.forEach(player => {
					if (player.pause) player.pause();
					player.style.display = 'none';
				});
				playerToShow.style.display = 'block';
			
				// Adjust status text based on media type
				if (mimeType.startsWith('video')) {
					statusText.textContent = 'Video playing...'; // Example status message for video
					fullscreenButton.style.display = 'inline-block';
					reloadButton.style.display = 'inline-block';
					fullscreenButton.onclick = () => enterFullScreen(playerToShow);
					reloadButton.onclick = () => playMedia(latestMedia.url, latestMedia.mimeType);
				} else if (mimeType.startsWith('audio')) {
					statusText.textContent = 'Audio playing...'; // Example status message for audio
					fullscreenButton.style.display = 'none';
					reloadButton.style.display = 'none';
				} else if (mimeType.startsWith('image')) {
					statusText.textContent = 'Click the photo for full screen';
					fullscreenButton.style.display = 'none';
					reloadButton.style.display = 'none';
				} else {
					statusText.textContent = ''; // Clear or set a default message for unsupported media types
					fullscreenButton.style.display = 'none';
					reloadButton.style.display = 'none';
				}
			};

			const loadAndPlayMedia = (player, url) => {
				const uniqueUrl = url + '?nocache=' + new Date().getTime();
				player.src = uniqueUrl;
				player.load();
				player.play().catch(error => {
					console.error('Error playing media: ', error);
					statusText.textContent = 'Error playing media. Retrying...';
					setTimeout(() => playMedia(url, latestMedia.mimeType), 3000);
				});
			};
		
			const loadImage = (imageElement, url) => {
				const uniqueUrl = url + '?nocache=' + new Date().getTime();
				imageElement.src = uniqueUrl;
			};

			const enterFullScreen = (element) => {
				if (element.requestFullscreen) {
					element.requestFullscreen();
				} else if (element.webkitRequestFullscreen) { /* Safari */
					element.webkitRequestFullscreen();
				} else if (element.msRequestFullscreen) { /* IE11 */
					element.msRequestFullscreen();
				} else if (element.mozRequestFullScreen) { /* Firefox */
					element.mozRequestFullScreen();
				}
			};
		
			imageViewer.addEventListener('click', () => {
				enterFullScreen(imageViewer);
			});
		
			setupWebSocket();
		});

    </script>
</body>
</html>
`
	t, err := template.New("webpage").Parse(tmpl)
	if err != nil {
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}

	err = t.Execute(w, map[string]interface{}{
		"ChatID": chatID,
	})
	if err != nil {
		http.Error(w, "Failed to execute template", http.StatusInternalServerError)
		return
	}
}
