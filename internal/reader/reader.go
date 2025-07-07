package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
)

const (
	chunkSize            = int64(512 * 1024) // 512KB
	maxRequestsPerSecond = 30                // Max number of requests per second.
	maxRetries           = 5                 // Maximum number of retries.
	baseDelay            = time.Second       // Initial delay for exponential backoff.
	maxDelay             = 60 * time.Second  // Maximum delay for backoff.
)

var (
	rateLimiter = time.NewTicker(time.Second / maxRequestsPerSecond)
	mu          sync.Mutex
)

type telegramReader struct {
	ctx           context.Context
	log           *log.Logger
	client        *gotgproto.Client
	location      tg.InputFileLocationClass // Changed to be more general
	start         int64                     // Requested start byte for the stream
	end           int64                     // Requested end byte for the stream
	next          func() ([]byte, error)    // Function to get the next raw chunk from Telegram/cache
	buffer        []byte                    // Current buffer holding raw data from a Telegram chunk
	bytesread     int64                     // Total bytes read from the *requested* stream range
	chunkSize     int64                     // Fixed size for Telegram API requests and internal chunking
	i             int64                     // Index within the current `buffer`
	contentLength int64                     // Total size of the file on Telegram
	cache         *BinaryCache
}

// NewTelegramReader initializes a new telegramReader with the given parameters, including a BinaryCache.
// It now accepts tg.InputFileLocationClass for flexibility to stream documents and photos.
func NewTelegramReader(ctx context.Context, client *gotgproto.Client, location tg.InputFileLocationClass, start int64, end int64, contentLength int64, cache *BinaryCache, logger *log.Logger) (io.ReadCloser, error) {
	r := &telegramReader{
		ctx:           ctx,
		log:           logger,
		location:      location,
		client:        client,
		start:         start,
		end:           end,
		chunkSize:     chunkSize,
		contentLength: contentLength,
		cache:         cache,
	}
	r.log.Println("Initializing TelegramReader.")
	r.next = r.partStream() // Initialize the function to get the next raw chunk
	return r, nil
}

// Close implements the io.Closer interface but doesn't perform any actions.
func (*telegramReader) Close() error {
	return nil
}

// Read reads the next chunk of data into the provided byte slice `p`.
// This function handles applying the requested `start` offset and `end` limit to the raw chunks.
func (r *telegramReader) Read(p []byte) (n int, err error) {
	// Calculate the total number of bytes we need to serve for the requested range.
	totalBytesToServe := r.end - r.start + 1

	// If we have already served all requested bytes, return EOF.
	if r.bytesread >= totalBytesToServe {
		return 0, io.EOF
	}

	// If the current buffer is exhausted, fetch the next raw chunk from Telegram/cache.
	if r.i >= int64(len(r.buffer)) {
		r.buffer, err = r.next() // Fetch the next raw Telegram chunk (full chunk from Telegram's perspective)
		if err != nil {
			return 0, err
		}
		r.i = 0 // Reset internal buffer index for the new buffer.

		// If this is the very first chunk being processed, apply the initial offset (`r.start`).
		// The `r.buffer` currently holds data starting from `initialAlignedOffset`.
		// We need to skip `r.start - initialAlignedOffset` bytes from the beginning of `r.buffer`.
		if r.bytesread == 0 {
			initialAlignedOffset := r.start - (r.start % r.chunkSize)
			bytesToSkip := r.start - initialAlignedOffset
			if bytesToSkip < int64(len(r.buffer)) {
				r.buffer = r.buffer[bytesToSkip:]
			} else {
				// This implies r.start is beyond the first fetched chunk, or beyond file end.
				// This should ideally be caught by `totalBytesToServe` checks earlier.
				r.log.Printf("Read: Bytes to skip (%d) for initial offset (%d) exceeds first buffer length (%d). Likely EOF or invalid range.", bytesToSkip, r.start, len(r.buffer))
				return 0, io.EOF
			}
		}
	}

	// Determine how many bytes to copy from the current buffer into `p`.
	// Max bytes to copy is limited by:
	// 1. The capacity of `p` (`len(p)`).
	// 2. Remaining bytes in `r.buffer` (`len(r.buffer) - r.i`).
	// 3. Remaining bytes needed for the entire requested range (`totalBytesToServe - r.bytesread`).
	bytesLeftInBuffer := int64(len(r.buffer)) - r.i
	bytesRemainingForRequest := totalBytesToServe - r.bytesread

	// Fix: Cast len(p) to int64
	bytesToCopy := minInt64(int64(len(p)), bytesLeftInBuffer, bytesRemainingForRequest)

	n = copy(p, r.buffer[r.i:r.i+bytesToCopy])

	r.i += int64(n)
	r.bytesread += int64(n)

	return n, nil
}

// chunk requests a file chunk from the Telegram API starting at the specified offset or retrieves it from the cache.
func (r *telegramReader) chunk(offset int64, limit int64) ([]byte, error) {
	// Extract a consistent location ID for caching from the InputFileLocationClass.
	var locationID int64
	switch l := r.location.(type) {
	case *tg.InputDocumentFileLocation:
		locationID = l.ID
	case *tg.InputPhotoFileLocation:
		locationID = l.ID
	default:
		return nil, fmt.Errorf("unsupported location type for caching: %T", r.location)
	}

	chunkID := offset / r.chunkSize
	cachedChunk, err := r.cache.readChunk(locationID, chunkID) // Use the extracted locationID
	if err == nil {
		r.log.Printf("Cache hit for chunk %d (location %d).", chunkID, locationID)
		return cachedChunk, nil
	}

	r.log.Printf("Cache miss for chunk %d (location %d), requesting from Telegram API.", chunkID, locationID)

	// If not in cache, request it from Telegram
	req := &tg.UploadGetFileRequest{
		Offset:   offset,
		Limit:    int(limit),
		Location: r.location, // This now accepts tg.InputFileLocationClass directly
	}
	return r.downloadAndCacheChunk(req, chunkID)
}

// downloadAndCacheChunk combines rate limiting and exponential backoff.
func (r *telegramReader) downloadAndCacheChunk(req *tg.UploadGetFileRequest, chunkID int64) ([]byte, error) {
	delay := baseDelay // Start with the base delay for exponential backoff.

	// Extract locationID for logging and caching consistently
	var locationID int64
	switch l := req.Location.(type) {
	case *tg.InputDocumentFileLocation:
		locationID = l.ID
	case *tg.InputPhotoFileLocation:
		locationID = l.ID
	default:
		// This should not happen if chunk() handles it first, but as a safeguard.
		return nil, fmt.Errorf("unsupported location type for caching in downloadAndCacheChunk: %T", req.Location)
	}

	for retryCount := 0; retryCount < maxRetries; retryCount++ {
		// Rate limiting: Wait for the rate limiter to allow a new request.
		mu.Lock()
		<-rateLimiter.C
		mu.Unlock()

		res, err := r.client.API().UploadGetFile(r.ctx, req) // This call remains the same, accepting InputFileLocationClass
		if err != nil {
			// Handle FLOOD_WAIT error by sleeping for the specified time and retrying.
			if floodWait, ok := isFloodWaitError(err); ok {
				r.log.Printf("FLOOD_WAIT error: retrying in %d seconds.", floodWait)
				time.Sleep(time.Duration(floodWait) * time.Second)
				continue
			}

			// Handle transient errors with exponential backoff.
			if isTransientError(err) {
				r.log.Printf("Transient error: %v, retrying in %v", err, delay)
				time.Sleep(delay)
				delay = minDuration(delay*2, maxDelay) // Increase delay with exponential backoff, capping at maxDelay.
				continue
			}

			// Return non-transient errors without retrying.
			r.log.Printf("Error during chunk download: %v", err)
			return nil, err
		}

		switch result := res.(type) {
		case *tg.UploadFile:
			chunkData := result.Bytes
			err = r.cache.writeChunk(locationID, chunkID, chunkData) // Use the extracted locationID
			if err != nil {
				r.log.Printf("Error writing chunk to cache (location %d, chunk %d): %v", locationID, chunkID, err)
			}
			return chunkData, nil
		default:
			return nil, fmt.Errorf("Unexpected response type: %T", result)
		}
	}

	// If all retries are exhausted, return an error.
	return nil, fmt.Errorf("failed to download chunk %d for location %d after %d retries", chunkID, locationID, maxRetries)
}

// partStream returns a function that fetches raw file chunks from the Telegram API.
// It determines the correct offset and limit for the API call.
func (r *telegramReader) partStream() func() ([]byte, error) {
	// Calculate the starting file offset, aligned to `chunkSize`.
	// This is the first byte offset we will request from Telegram.
	currentAlignedFileOffset := r.start - (r.start % r.chunkSize)

	return func() ([]byte, error) {
		// If we've already tried to read past the end of the file, return EOF.
		if currentAlignedFileOffset >= r.contentLength {
			return nil, io.EOF
		}

		// Calculate the limit for the current Telegram API request.
		// It should be `chunkSize`, but capped by `contentLength` to avoid reading beyond the file.
		limit := r.chunkSize
		if currentAlignedFileOffset+limit > r.contentLength {
			limit = r.contentLength - currentAlignedFileOffset
		}

		if limit <= 0 { // No more data to fetch (e.g., if content length is 0 or we're at the very end)
			return nil, io.EOF
		}

		// Fetch the raw chunk from Telegram or cache.
		chunkData, err := r.chunk(currentAlignedFileOffset, limit)
		if err != nil {
			return nil, err
		}

		if len(chunkData) == 0 {
			// If we get an empty chunk but haven't reached end of file, it's unexpected.
			// Treat as EOF or error, depending on desired strictness.
			r.log.Printf("Received empty chunk from Telegram API for offset %d, limit %d, but content length is %d. Treating as EOF.", currentAlignedFileOffset, limit, r.contentLength)
			return nil, io.EOF
		}

		// Advance the offset for the next chunk request.
		currentAlignedFileOffset += r.chunkSize

		// Return the raw chunk. The `Read` method will handle slicing for the requested range.
		return chunkData, nil
	}
}

// isFloodWaitError checks if the error is a FLOOD_WAIT error and returns the wait time if true.
func isFloodWaitError(err error) (int, bool) {
	errText := err.Error()
	matched, _ := regexp.MatchString(`FLOOD_WAIT \(\d+\)`, errText)
	if matched {
		re := regexp.MustCompile(`FLOOD_WAIT \((\d+)\)`)
		match := re.FindStringSubmatch(errText)
		if len(match) > 1 {
			waitTime, err := strconv.Atoi(match[1])
			if err == nil {
				return waitTime, true
			}
		}
	}
	return 0, false
}

// isTransientError checks if an error is transient (e.g., network issues), meaning it might be resolved by retrying.
func isTransientError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}

	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}

	return false
}

// minDuration returns the minimum of two time.Duration values.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// minInt64 returns the minimum of multiple int64 values.
func minInt64(vals ...int64) int64 {
	if len(vals) == 0 {
		return 0 // Or handle as an error
	}
	minVal := vals[0]
	for _, v := range vals {
		if v < minVal {
			minVal = v
		}
	}
	return minVal
}
