package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
	"webBridgeBot/internal/reader"
	"webBridgeBot/internal/utils"

	"github.com/gorilla/mux"
	"github.com/gotd/td/tg"
)

// handleStream handles the file streaming from Telegram
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vars := mux.Vars(r)
	messageIDStr := vars["messageID"]
	authHash := vars["hash"]

	s.logger.Printf("Received request to stream file with message ID: %s from client %s", messageIDStr, r.RemoteAddr)

	if s.config.DebugMode {
		s.logger.Debugf("Stream request details - MessageID: %s, Hash: %s, Range: %s, User-Agent: %s",
			messageIDStr, authHash, r.Header.Get("Range"), r.Header.Get("User-Agent"))
	}

	// Parse and validate message ID
	messageID, err := strconv.Atoi(messageIDStr)
	if err != nil {
		s.logger.Printf("Invalid message ID '%s' received from client %s", messageIDStr, r.RemoteAddr)
		http.Error(w, "Invalid message ID format", http.StatusBadRequest)
		return
	}

	// Fetch the file information from Telegram (or cache)
	if s.config.DebugMode {
		s.logger.Debugf("Fetching file information for message ID %d", messageID)
	}

	file, err := utils.FileFromMessage(ctx, s.tgClient, messageID)
	if err != nil {
		s.logger.Printf("Error fetching file for message ID %d: %v", messageID, err)
		if s.config.DebugMode {
			s.logger.Debugf("File fetch failed for message ID %d: %v", messageID, err)
		}
		http.Error(w, "Unable to retrieve file for the specified message", http.StatusBadRequest)
		return
	}

	if s.config.DebugMode {
		s.logger.Debugf("File retrieved: %s (%d bytes)", file.FileName, file.FileSize)
	}

	// Hash verification
	expectedHash := utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID)
	if !utils.CheckHash(authHash, expectedHash, s.config.HashLength) {
		s.logger.Printf("Hash verification failed for message ID %d from client %s", messageID, r.RemoteAddr)
		if s.config.DebugMode {
			s.logger.Debugf("Hash mismatch - Expected: %s..., Got: %s", expectedHash[:10], authHash)
		}
		http.Error(w, "Invalid authentication hash", http.StatusBadRequest)
		return
	}

	if s.config.DebugMode {
		s.logger.Debugf("Hash verification passed for message ID %d", messageID)
	}

	contentLength := file.FileSize

	// Default range values for full content
	var start, end int64 = 0, contentLength - 1

	// Process range header if present
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		s.logger.Printf("Range header received for message ID %d: %s", messageID, rangeHeader)
		if strings.HasPrefix(rangeHeader, "bytes=") {
			ranges := strings.Split(rangeHeader[len("bytes="):], "-")
			if len(ranges) == 2 {
				if ranges[0] != "" {
					start, err = strconv.ParseInt(ranges[0], 10, 64)
					if err != nil {
						s.logger.Printf("Invalid start range value for message ID %d: %v", messageID, err)
						http.Error(w, "Invalid range start value", http.StatusBadRequest)
						return
					}
				}
				if ranges[1] != "" {
					end, err = strconv.ParseInt(ranges[1], 10, 64)
					if err != nil {
						s.logger.Printf("Invalid end range value for message ID %d: %v", messageID, err)
						http.Error(w, "Invalid range end value", http.StatusBadRequest)
						return
					}
				}
			}
		}
	}

	// Validate the requested range
	if start > end || start < 0 || end >= contentLength {
		s.logger.Printf("Requested range not satisfiable for message ID %d: start=%d, end=%d, contentLength=%d", messageID, start, end, contentLength)
		http.Error(w, "Requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// For large video files (>100MB) without a Range header, force a small initial chunk
	// This must be done BEFORE creating the TelegramReader to avoid Content-Length mismatch
	const largeFileThreshold = 100 * 1024 * 1024 // 100MB
	if rangeHeader == "" && contentLength > largeFileThreshold && strings.HasPrefix(file.MimeType, "video/") {
		// Send only the first 5MB to encourage range requests
		end = 5*1024*1024 - 1
		if end >= contentLength {
			end = contentLength - 1
		}
		s.logger.Printf("Large video file detected (%d bytes). Serving initial chunk for message ID %d: bytes 0-%d/%d", contentLength, messageID, end, contentLength)
	}

	// Create a TelegramReader to stream the content with the correct range
	lr, err := reader.NewTelegramReader(context.Background(), s.tgClient, file.Location, start, end, contentLength, s.config.BinaryCache, s.logger, s.config.DebugMode, s.config.MaxRetries, s.config.RetryBaseDelay, s.config.MaxRetryDelay)
	if err != nil {
		s.logger.Printf("Error creating Telegram reader for message ID %d: %v", messageID, err)
		http.Error(w, "Failed to initialize file stream", http.StatusInternalServerError)
		return
	}
	defer lr.Close()

	// Send appropriate headers and stream the content
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", file.MimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, file.FileName))

	if rangeHeader == "" && contentLength > largeFileThreshold && strings.HasPrefix(file.MimeType, "video/") {
		// We already adjusted 'end' above, now set headers for partial content
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, contentLength))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else if rangeHeader != "" {
		s.logger.Printf("Serving partial content for message ID %d: bytes %d-%d/%d", messageID, start, end, contentLength)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, contentLength))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		s.logger.Printf("Serving full content for message ID %d", messageID)
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
		w.WriteHeader(http.StatusOK)
	}

	// Stream the content to the client
	if _, err := io.Copy(w, lr); err != nil {
		// These errors are expected if the client disconnects
		if isClientDisconnectError(err) {
			if s.config.DebugMode {
				s.logger.Debugf("Client disconnected during stream for message ID %d from %s", messageID, r.RemoteAddr)
			}
		} else {
			s.logger.Printf("Error streaming content for message ID %d: %v", messageID, err)
		}
	}
}

// handlePlayer serves the HTML player page and adds authorization
func (s *Server) handlePlayer(w http.ResponseWriter, r *http.Request) {
	s.logger.Printf("Received request for player: %s", r.URL.Path)

	chatID, err := parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Authorize user based on chatID
	userInfo, err := s.userRepository.GetUserInfo(chatID)
	if err != nil || !userInfo.IsAuthorized {
		http.Error(w, "Unauthorized access to player. Please start the bot first.", http.StatusUnauthorized)
		s.logger.Printf("Unauthorized player access attempt for chatID %d: User not found or not authorized (%v)", chatID, err)
		return
	}

	t, err := template.ParseFiles(tmplPath)
	if err != nil {
		s.logger.Printf("Error loading template: %v", err)
		http.Error(w, "Failed to load template", http.StatusInternalServerError)
		return
	}

	if err := t.Execute(w, map[string]interface{}{"User": userInfo}); err != nil {
		s.logger.Printf("Error rendering template: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
	}
}

// handleAvatar serves a user's Telegram profile photo
func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	chatID, err := parseChatID(mux.Vars(r))
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Authorize user based on chatID
	userInfo, err := s.userRepository.GetUserInfo(chatID)
	if err != nil || !userInfo.IsAuthorized {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	// Resolve InputUser from peer storage to query photos
	peer := s.tgCtx.PeerStorage.GetInputPeerById(chatID)

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

	// Fetch latest user photos (limit 1) with retry for FLOOD_WAIT
	var photosRes tg.PhotosPhotosClass
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		photosRes, err = s.tgClient.API().PhotosGetUserPhotos(ctx, &tg.PhotosGetUserPhotosRequest{
			UserID: inputUser,
			Offset: 0,
			MaxID:  0,
			Limit:  1,
		})
		if err == nil {
			break
		}

		// Check for FLOOD_WAIT error
		if floodWait, isFlood := utils.ExtractFloodWait(err); isFlood {
			if attempt < maxRetries-1 {
				s.logger.Printf("Avatar: FLOOD_WAIT for %d, waiting %d seconds (attempt %d/%d)", chatID, floodWait, attempt+1, maxRetries)
				time.Sleep(time.Duration(floodWait) * time.Second)
				continue
			}
		}

		// If not FLOOD_WAIT or last attempt, break
		break
	}

	if err != nil {
		s.logger.Printf("Avatar: failed PhotosGetUserPhotos for %d after retries: %v", chatID, err)
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

	// Choose a reasonable thumbnail size type
	thumbType := "x"
	var sizeBytes int
	for _, s := range photo.Sizes {
		if ps, ok := s.(*tg.PhotoSize); ok {
			if ps.Type == "x" {
				thumbType = ps.Type
				sizeBytes = ps.Size
				break
			}
			if sizeBytes == 0 && ps.Size > 0 {
				thumbType = ps.Type
				sizeBytes = ps.Size
			}
		}
	}
	if sizeBytes <= 0 {
		sizeBytes = 256 * 1024 // 256 KiB heuristic
	}

	// Build location for the chosen thumbnail
	location := &tg.InputPhotoFileLocation{
		ID:            photo.ID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		ThumbSize:     thumbType,
	}

	// Stream using existing Telegram reader
	start := int64(0)
	end := int64(sizeBytes - 1)
	if end < 0 {
		end = 0
	}

	rc, err := reader.NewTelegramReader(ctx, s.tgClient, location, start, end, int64(sizeBytes), s.config.BinaryCache, s.logger, s.config.DebugMode, s.config.MaxRetries, s.config.RetryBaseDelay, s.config.MaxRetryDelay)
	if err != nil {
		s.logger.Printf("Avatar: reader init failed for %d: %v", chatID, err)
		http.NotFound(w, r)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if sizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(sizeBytes))
	}

	if _, err := io.Copy(w, rc); err != nil {
		// Only log unexpected errors, client disconnects are normal
		if !isClientDisconnectError(err) {
			s.logger.Printf("Avatar: stream error for %d: %v", chatID, err)
		}
	}
}

// handleProxy proxies external URLs to bypass CORS restrictions
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	externalURL := r.URL.Query().Get("url")
	if externalURL == "" {
		http.Error(w, "Missing 'url' parameter", http.StatusBadRequest)
		return
	}

	if s.config.DebugMode {
		s.logger.Debugf("Proxy request for URL: %s", externalURL)
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Fetch the external resource
	resp, err := client.Get(externalURL)
	if err != nil {
		s.logger.Printf("Error fetching external URL %s: %v", externalURL, err)
		http.Error(w, fmt.Sprintf("Failed to fetch resource: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Check if the response is HTML (file hosting page) instead of media
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" && strings.Contains(strings.ToLower(contentType), "text/html") {
		s.logger.Printf("Warning: External URL %s returned HTML (file hosting page) instead of media. Content-Type: %s", externalURL, contentType)
		if s.config.DebugMode {
			s.logger.Debugf("This appears to be a file hosting page, not a direct media URL")
		}
	}

	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, Content-Type")

	// Copy content-type and other relevant headers
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if contentLength := resp.Header.Get("Content-Length"); contentLength != "" {
		w.Header().Set("Content-Length", contentLength)
	}
	if acceptRanges := resp.Header.Get("Accept-Ranges"); acceptRanges != "" {
		w.Header().Set("Accept-Ranges", acceptRanges)
	}

	// Handle range requests
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" && resp.StatusCode == http.StatusOK {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(resp.StatusCode)
	}

	// Stream the response
	_, err = io.Copy(w, resp.Body)
	if err != nil && s.config.DebugMode {
		s.logger.Debugf("Error streaming proxied content: %v", err)
	}

	if s.config.DebugMode {
		s.logger.Debugf("Proxy completed for URL: %s", externalURL)
	}
}

// isClientDisconnectError checks if an error is caused by client disconnection
// These are expected errors when clients close connections (e.g., seeking in videos, closing browser tabs)
func isClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}

	// Check for specific syscall errors
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}

	// Check error message strings for wrapped errors that errors.Is() might miss
	errMsg := err.Error()
	disconnectPatterns := []string{
		"broken pipe",
		"connection reset by peer",
		"connection reset",
		"client disconnected",
		"write: connection reset",
		"readfrom tcp",
	}

	for _, pattern := range disconnectPatterns {
		if strings.Contains(strings.ToLower(errMsg), pattern) {
			return true
		}
	}

	return false
}
