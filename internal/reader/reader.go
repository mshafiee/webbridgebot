package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
	"webBridgeBot/internal/logger"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
)

const (
	// apiAlignment is the smallest allowed block size for offset and limit in Telegram API's upload.getFile method.
	apiAlignment = int64(4096)
	// telegramMaxLimit is the absolute maximum 'limit' allowed by Telegram API's upload.getFile method.
	telegramMaxLimit = int64(512 * 1024)

	// preferredChunkSize is our desired chunk size for internal processing and caching.
	// It must be a multiple of apiAlignment and a power-of-2 multiple of apiAlignment.
	// 256 KiB (262144 bytes) is 4096 * 64 (where 64 is 2^6), which is a safe value.
	preferredChunkSize = int64(256 * 1024)

	maxRequestsPerSecond = 30
)

var (
	rateLimiter = time.NewTicker(time.Second / maxRequestsPerSecond)
	mu          sync.Mutex

	// Circuit breaker for chunk downloads
	chunkFailures      = make(map[string]*circuitBreakerState)
	chunkFailuresMutex sync.RWMutex
)

// circuitBreakerState tracks failure state for a specific chunk
type circuitBreakerState struct {
	failures         int
	lastFailure      time.Time
	blockedUntil     time.Time
	consecutiveFails int
}

const (
	circuitBreakerThreshold = 3                // Number of consecutive failures before opening circuit
	circuitBreakerTimeout   = 1 * time.Minute  // How long to block retries after circuit opens (reduced for faster recovery)
	circuitBreakerReset     = 30 * time.Second // Reset failure count after this period of no failures
)

// telegramClient defines the interface for the parts of the Telegram client that we use.
// This allows for mocking in tests and is satisfied by *gotgproto.Client.
type telegramClient interface {
	API() *tg.Client
}

type telegramReader struct {
	ctx           context.Context
	log           *logger.Logger
	client        telegramClient
	location      tg.InputFileLocationClass
	start         int64
	end           int64
	next          func() ([]byte, error)
	buffer        []byte
	bytesread     int64
	chunkSize     int64
	i             int64
	contentLength int64
	cache         *BinaryCache
	debugMode     bool

	// Configurable retry settings
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	apiTimeout time.Duration

	// Dynamic chunk sizing for handling timeouts
	consecutiveTimeouts int
	requestLimit        int64
	successfulChunks    int
}

func NewTelegramReader(ctx context.Context, client *gotgproto.Client, location tg.InputFileLocationClass, start int64, end int64, contentLength int64, cache *BinaryCache, log *logger.Logger, debugMode bool, maxRetries int, retryBaseDelay int, maxRetryDelay int, apiTimeout int) (io.ReadCloser, error) {
	r := &telegramReader{
		ctx:                 ctx,
		log:                 log,
		location:            location,
		client:              client,
		start:               start,
		end:                 end,
		chunkSize:           preferredChunkSize,
		contentLength:       contentLength,
		cache:               cache,
		debugMode:           debugMode,
		maxRetries:          maxRetries,
		baseDelay:           time.Duration(retryBaseDelay) * time.Second,
		maxDelay:            time.Duration(maxRetryDelay) * time.Second,
		apiTimeout:          time.Duration(apiTimeout) * time.Second,
		requestLimit:        preferredChunkSize,
		consecutiveTimeouts: 0,
		successfulChunks:    0,
	}
	if r.debugMode {
		r.log.Debug("Initializing TelegramReader.")
	}
	r.next = r.partStream()
	return r, nil
}

func (*telegramReader) Close() error {
	return nil
}

func (r *telegramReader) Read(p []byte) (n int, err error) {
	totalBytesToServe := r.end - r.start + 1

	if r.bytesread >= totalBytesToServe {
		return 0, io.EOF
	}

	// If the internal buffer is exhausted, fetch the next chunk.
	// r.i tracks the current read position within r.buffer.
	if r.i >= int64(len(r.buffer)) {
		r.buffer, err = r.next()
		if err != nil {
			return 0, err
		}
		r.i = 0 // Reset internal buffer index
	}

	// Calculate the initial offset into the first received chunk.
	// This is only applied once for the very first read from the stream.
	// r.bytesread tracks how much data has been returned to the caller of Read.
	if r.bytesread == 0 && len(r.buffer) > 0 {
		// The initial API request offset is aligned to r.chunkSize, but the
		// requested range (r.start) might not be. We need to skip bytes from
		// the beginning of the *first fetched chunk* to match the exact 'start'
		// byte requested by the HTTP range header.
		initialAlignedRequestOffset := r.start - (r.start % r.chunkSize)
		bytesToSkipInFirstChunk := r.start - initialAlignedRequestOffset

		if bytesToSkipInFirstChunk < int64(len(r.buffer)) {
			r.buffer = r.buffer[bytesToSkipInFirstChunk:]
		} else {
			// This means the requested start is beyond the first fetched block.
			r.log.Printf("Read: Bytes to skip (%d) for initial offset (%d) exceeds first buffer length (%d). Likely EOF or invalid range.", bytesToSkipInFirstChunk, r.start, len(r.buffer))
			return 0, io.EOF
		}
	}

	bytesLeftInBuffer := int64(len(r.buffer)) - r.i
	bytesRemainingForRequest := totalBytesToServe - r.bytesread

	// Determine how many bytes to copy: min of:
	// 1. remaining capacity in destination slice `p`
	// 2. bytes left in internal buffer `r.buffer`
	// 3. bytes remaining to satisfy the overall requested range `totalBytesToServe`
	bytesToCopy := minInt64(int64(len(p)), bytesLeftInBuffer, bytesRemainingForRequest)

	n = copy(p, r.buffer[r.i:r.i+bytesToCopy])

	r.i += int64(n)
	r.bytesread += int64(n)

	return n, nil
}

func (r *telegramReader) chunk(offset int64, limit int64) ([]byte, error) {
	var locationID int64
	switch l := r.location.(type) {
	case *tg.InputDocumentFileLocation:
		locationID = l.ID
	case *tg.InputPhotoFileLocation:
		locationID = l.ID
	default:
		return nil, fmt.Errorf("unsupported location type for caching: %T", r.location)
	}

	// The cache is structured around `r.chunkSize` (preferredChunkSize) logical chunks.
	// The `offset` here is the `currentAPIOffset` from `partStream`, which is aligned to `r.chunkSize`.
	cacheChunkID := offset / r.chunkSize

	// Attempt to read the entire logical chunk from cache first.
	cachedLogicalChunk, err := r.cache.readChunk(locationID, cacheChunkID)
	if err == nil {
		if r.debugMode {
			r.log.Debugf("Cache hit for logical chunk %d (location %d).", cacheChunkID, locationID)
		}
		// If cached data is found, ensure we return only up to the requested `limit`.
		if int64(len(cachedLogicalChunk)) >= limit {
			return cachedLogicalChunk[:limit], nil
		}
		// If cached data is smaller than the requested limit, it means it's the last chunk.
		return cachedLogicalChunk, nil
	}

	if r.debugMode {
		r.log.Debugf("Cache miss for logical chunk %d (location %d), requesting from Telegram API.", cacheChunkID, locationID)
	}

	req := &tg.UploadGetFileRequest{
		Offset:   offset,
		Limit:    int(limit),
		Location: r.location,
	}
	return r.downloadAndCacheChunk(req, cacheChunkID)
}

func (r *telegramReader) downloadAndCacheChunk(req *tg.UploadGetFileRequest, cacheChunkID int64) ([]byte, error) {
	delay := r.baseDelay

	var locationID int64
	switch l := req.Location.(type) {
	case *tg.InputDocumentFileLocation:
		locationID = l.ID
	case *tg.InputPhotoFileLocation:
		locationID = l.ID
	default:
		return nil, fmt.Errorf("unsupported location type for caching in downloadAndCacheChunk: %T", req.Location)
	}

	// Check circuit breaker before attempting download
	chunkKey := getChunkKey(locationID, int64(req.Offset))
	if checkCircuitBreaker(chunkKey, r.log) {
		return nil, fmt.Errorf("circuit breaker open for chunk at offset %d (location %d): too many recent failures", req.Offset, locationID)
	}

	// Apply dynamic request limit (may be smaller than req.Limit due to timeouts)
	actualLimit := minInt64(r.requestLimit, int64(req.Limit))
	req.Limit = int(actualLimit)

	for retryCount := 0; retryCount < r.maxRetries; retryCount++ {
		mu.Lock()
		<-rateLimiter.C
		mu.Unlock()

		if r.debugMode {
			r.log.Debugf("Sending UploadGetFileRequest for chunk %d (location %d): Offset=%d, Limit=%d, LocationType=%T",
				cacheChunkID, locationID, req.Offset, req.Limit, req.Location)
		}

		// Add timeout to prevent hanging on slow API calls
		timeoutCtx, cancel := context.WithTimeout(r.ctx, r.apiTimeout)
		res, err := r.client.API().UploadGetFile(timeoutCtx, req)
		cancel() // Release resources immediately after call

		if err != nil {
			if floodWait, ok := isFloodWaitError(err); ok {
				r.log.Printf("FLOOD_WAIT error: retrying in %d seconds (attempt %d/%d)", floodWait, retryCount+1, r.maxRetries)
				time.Sleep(time.Duration(floodWait) * time.Second)
				continue
			}

			if isTransientError(err) {
				// Check if this is specifically a timeout error
				isTimeout := errors.Is(err, context.DeadlineExceeded) ||
					regexp.MustCompile(`rpc error code -503`).MatchString(err.Error())

				if isTimeout {
					r.consecutiveTimeouts++

					// Reduce chunk size after 3 consecutive timeouts
					if r.consecutiveTimeouts >= 3 && r.requestLimit > 65536 { // 64KB minimum
						oldLimit := r.requestLimit
						r.requestLimit = r.requestLimit / 2
						if r.requestLimit < 65536 {
							r.requestLimit = 65536
						}
						r.log.Printf("Reducing chunk size from %d to %d bytes after %d consecutive timeouts",
							oldLimit, r.requestLimit, r.consecutiveTimeouts)
						// Update the request limit for this attempt
						actualLimit := minInt64(r.requestLimit, int64(req.Limit))
						req.Limit = int(actualLimit)
					}
				}

				r.log.Printf("Transient error: %v, retrying in %v (attempt %d/%d)", err, delay, retryCount+1, r.maxRetries)
				time.Sleep(delay)
				delay = minDuration(delay*2, r.maxDelay)
				continue
			}

			r.log.Printf("Error during chunk download: %v (attempt %d/%d)", err, retryCount+1, r.maxRetries)
			// Record failure for circuit breaker
			recordChunkFailure(chunkKey, r.log)
			return nil, err
		}

		switch result := res.(type) {
		case *tg.UploadFile:
			chunkData := result.Bytes
			// Record success for circuit breaker
			recordChunkSuccess(chunkKey)

			// Reset timeout counter and increment successful chunks on success
			r.consecutiveTimeouts = 0
			r.successfulChunks++

			// Restore chunk size after 5 successful downloads
			if r.successfulChunks >= 5 && r.requestLimit < preferredChunkSize {
				r.log.Printf("Restored chunk size from %d to %d bytes after %d successful downloads",
					r.requestLimit, preferredChunkSize, r.successfulChunks)
				r.requestLimit = preferredChunkSize
				r.successfulChunks = 0
			}

			// Log successful retry if this wasn't the first attempt
			if retryCount > 0 {
				r.log.Printf("Successfully downloaded chunk after %d attempts", retryCount+1)
			}

			// Write the downloaded chunk to cache. The cache implementation handles
			// data that is smaller than its internal fixed chunk size.
			err = r.cache.writeChunk(locationID, cacheChunkID, chunkData)
			if err != nil {
				r.log.Printf("Error writing chunk to cache (location %d, chunk %d): %v", locationID, cacheChunkID, err)
			}
			return chunkData, nil
		default:
			return nil, fmt.Errorf("Unexpected response type: %T", result)
		}
	}

	// Record failure after exhausting all retries
	recordChunkFailure(chunkKey, r.log)
	return nil, fmt.Errorf("failed to download chunk %d for location %d after %d retries", cacheChunkID, locationID, r.maxRetries)
}

func (r *telegramReader) partStream() func() ([]byte, error) {
	// currentAPIOffset is the offset at which Telegram API requests will start.
	// It must be a multiple of r.chunkSize (preferredChunkSize) to align with our caching strategy.
	// The initial offset is aligned to the nearest r.chunkSize boundary below r.start.
	currentAPIOffset := r.start - (r.start % r.chunkSize)

	return func() ([]byte, error) {
		if currentAPIOffset >= r.contentLength {
			return nil, io.EOF
		}

		// Use dynamic requestLimit for API calls (may be reduced due to timeouts)
		// but keep offset alignment to r.chunkSize for cache consistency
		limitToRequest := r.requestLimit

		if limitToRequest > telegramMaxLimit {
			limitToRequest = telegramMaxLimit
		}

		if r.debugMode {
			r.log.Debugf("Requesting chunk: Offset=%d, Limit=%d (dynamic limit, cache-aligned to %d)",
				currentAPIOffset, limitToRequest, r.chunkSize)
		}

		chunkData, err := r.chunk(currentAPIOffset, limitToRequest)
		if err != nil {
			r.log.Warningf("Failed to fetch chunk at offset %d (limit %d): %v - returning empty data to maintain stream continuity", currentAPIOffset, limitToRequest, err)

			// Return empty chunk instead of error - causes brief glitch but playback continues
			// This is the key to resilient video playback
			emptyChunk := make([]byte, limitToRequest)
			currentAPIOffset += r.chunkSize
			return emptyChunk, nil
		}

		// If we get an empty chunk but expected more, it's an issue.
		if len(chunkData) == 0 && (r.contentLength-currentAPIOffset) > 0 {
			r.log.Printf("Received empty chunk from Telegram API for offset %d, but expected more bytes (remaining: %d). Treating as EOF.", currentAPIOffset, (r.contentLength - currentAPIOffset))
			return nil, io.EOF
		}

		// Advance the API offset by r.chunkSize to maintain alignment for subsequent
		// requests and cache indexing. The Read method handles consuming the correct
		// number of bytes from the potentially larger chunkData.
		currentAPIOffset += r.chunkSize

		return chunkData, nil
	}
}

func isFloodWaitError(err error) (int, bool) {
	errText := err.Error()
	re := regexp.MustCompile(`FLOOD_WAIT \((\d+)\)`)
	match := re.FindStringSubmatch(errText)
	if len(match) > 1 {
		waitTime, err := strconv.Atoi(match[1])
		if err == nil {
			return waitTime, true
		}
	}
	return 0, false
}

func isTransientError(err error) bool {
	// Check for specific syscall errors first, as they are a reliable indicator.
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}

	// Then check for context cancellation.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Check for Telegram API timeout errors (RPC error code -503)
	errText := err.Error()
	if regexp.MustCompile(`rpc error code -503`).MatchString(errText) {
		return true
	}

	// Check for other Telegram API transient errors (5xx errors)
	if regexp.MustCompile(`rpc error code -(5\d{2})`).MatchString(errText) {
		return true
	}

	// Finally, check the general net.Error interface for temporary/timeout conditions.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}

	return false
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func minInt64(vals ...int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	minVal := vals[0]
	for _, v := range vals[1:] {
		if v < minVal {
			minVal = v
		}
	}
	return minVal
}

// getChunkKey generates a unique key for a chunk based on location and offset
func getChunkKey(locationID int64, offset int64) string {
	return fmt.Sprintf("%d:%d", locationID, offset)
}

// checkCircuitBreaker checks if the circuit is open for a given chunk
// Returns true if the chunk should be blocked, false otherwise
func checkCircuitBreaker(chunkKey string, log *logger.Logger) bool {
	chunkFailuresMutex.RLock()
	state, exists := chunkFailures[chunkKey]
	chunkFailuresMutex.RUnlock()

	if !exists {
		return false
	}

	now := time.Now()

	// If circuit is open (blocked), check if timeout has expired
	if now.Before(state.blockedUntil) {
		log.Warningf("Circuit breaker OPEN for chunk %s: blocked until %v (attempt blocked)",
			chunkKey, state.blockedUntil.Format(time.RFC3339))
		return true
	}

	// If enough time has passed since last failure, reset the state
	if now.After(state.lastFailure.Add(circuitBreakerReset)) {
		chunkFailuresMutex.Lock()
		delete(chunkFailures, chunkKey)
		chunkFailuresMutex.Unlock()
		log.Infof("Circuit breaker RESET for chunk %s: failure history cleared", chunkKey)
		return false
	}

	return false
}

// recordChunkFailure records a failure for a chunk and potentially opens the circuit
func recordChunkFailure(chunkKey string, log *logger.Logger) {
	chunkFailuresMutex.Lock()
	defer chunkFailuresMutex.Unlock()

	state, exists := chunkFailures[chunkKey]
	if !exists {
		state = &circuitBreakerState{}
		chunkFailures[chunkKey] = state
	}

	state.failures++
	state.consecutiveFails++
	state.lastFailure = time.Now()

	// Open circuit if threshold exceeded
	if state.consecutiveFails >= circuitBreakerThreshold {
		state.blockedUntil = time.Now().Add(circuitBreakerTimeout)
		log.Warningf("Circuit breaker OPENED for chunk %s: %d consecutive failures, blocking for %v",
			chunkKey, state.consecutiveFails, circuitBreakerTimeout)
	}
}

// recordChunkSuccess records a successful download and resets consecutive failures
func recordChunkSuccess(chunkKey string) {
	chunkFailuresMutex.Lock()
	defer chunkFailuresMutex.Unlock()

	if state, exists := chunkFailures[chunkKey]; exists {
		state.consecutiveFails = 0
		// Keep the total failure count for statistics, but reset consecutive failures
	}
}
