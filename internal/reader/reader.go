// internal/reader/reader.go

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
	// apiAlignment is the smallest allowed block size for offset and limit in Telegram API's upload.getFile method.
	apiAlignment = int64(4096)
	// telegramMaxLimit is the absolute maximum 'limit' allowed by Telegram API's upload.getFile method.
	telegramMaxLimit = int64(512 * 1024) // 524288 bytes (512 KiB)

	// preferredChunkSize is our desired chunk size for internal processing and caching.
	// It must be a multiple of apiAlignment and a power-of-2 multiple of apiAlignment.
	// 256 KiB (262144 bytes) = 4096 * 64 (where 64 is 2^6). This is a safe value.
	preferredChunkSize = int64(256 * 1024) // 262144 bytes (256 KiB)

	maxRequestsPerSecond = 30               // Max number of requests per second.
	maxRetries           = 5                // Maximum number of retries.
	baseDelay            = time.Second      // Initial delay for exponential backoff.
	maxDelay             = 60 * time.Second // Maximum delay for backoff.
)

var (
	rateLimiter = time.NewTicker(time.Second / maxRequestsPerSecond)
	mu          sync.Mutex
)

type telegramReader struct {
	ctx           context.Context
	log           *log.Logger
	client        *gotgproto.Client
	location      tg.InputFileLocationClass
	start         int64
	end           int64
	next          func() ([]byte, error)
	buffer        []byte
	bytesread     int64
	chunkSize     int64 // This instance field will hold the constant value preferredChunkSize
	i             int64
	contentLength int64
	cache         *BinaryCache
}

// NewTelegramReader initializes a new telegramReader.
func NewTelegramReader(ctx context.Context, client *gotgproto.Client, location tg.InputFileLocationClass, start int64, end int64, contentLength int64, cache *BinaryCache, logger *log.Logger) (io.ReadCloser, error) {
	r := &telegramReader{
		ctx:           ctx,
		log:           logger,
		location:      location,
		client:        client,
		start:         start,
		end:           end,
		chunkSize:     preferredChunkSize, // Initialize with the constant preferredChunkSize
		contentLength: contentLength,
		cache:         cache,
	}
	r.log.Println("Initializing TelegramReader.")
	r.next = r.partStream()
	return r, nil
}

// Close implements the io.Closer interface.
func (*telegramReader) Close() error {
	return nil
}

// Read reads data into the provided byte slice.
func (r *telegramReader) Read(p []byte) (n int, err error) {
	totalBytesToServe := r.end - r.start + 1

	if r.bytesread >= totalBytesToServe {
		return 0, io.EOF
	}

	// If the internal buffer is exhausted, fetch the next chunk.
	// r.i tracks the current read position within r.buffer.
	if r.i >= int64(len(r.buffer)) {
		r.buffer, err = r.next() // This call fetches data from Telegram/cache
		if err != nil {
			return 0, err
		}
		r.i = 0 // Reset internal buffer index
	}

	// Calculate the initial offset into the first received chunk.
	// This is only applied once for the very first read from the stream.
	// r.bytesread tracks how much data has been returned to the caller of Read.
	if r.bytesread == 0 && len(r.buffer) > 0 {
		// The initial API request offset (currentAPIOffset in partStream) is aligned to r.chunkSize.
		// However, the requested range (r.start) might not be.
		// We need to skip bytes from the beginning of the *first fetched chunk*
		// to match the exact 'start' byte requested by the HTTP range header.
		initialAlignedRequestOffset := r.start - (r.start % r.chunkSize)
		bytesToSkipInFirstChunk := r.start - initialAlignedRequestOffset

		if bytesToSkipInFirstChunk < int64(len(r.buffer)) {
			r.buffer = r.buffer[bytesToSkipInFirstChunk:]
		} else {
			// This means the requested start is beyond the first fetched block, or past content length.
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

// chunk requests a file chunk from Telegram or cache.
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

	// The cache is structured around `r.chunkSize` (preferredChunkSize, 256KB) logical chunks.
	// The `offset` here is the `currentAPIOffset` from `partStream`, which is aligned to `r.chunkSize`.
	cacheChunkID := offset / r.chunkSize

	// Attempt to read the entire logical chunk (of `r.chunkSize`) from cache first.
	// This assumes that when we write to cache, we always write a full `r.chunkSize` block
	// or the remaining tail of the file.
	cachedLogicalChunk, err := r.cache.readChunk(locationID, cacheChunkID)
	if err == nil {
		r.log.Printf("Cache hit for logical chunk %d (location %d).", cacheChunkID, locationID)
		// If cached data is found, ensure we return only up to the requested `limit` from this data.
		// Telegram often sends requests for standard chunk sizes, even if less is needed,
		// so we retrieve the cached data and trim it.
		if int64(len(cachedLogicalChunk)) >= limit {
			return cachedLogicalChunk[:limit], nil
		} else {
			// If cached data is smaller than the requested limit, it means it's the last chunk
			// or an incomplete cache entry. Return what's available.
			// No need to fetch from Telegram unless this indicates an error/corruption.
			return cachedLogicalChunk, nil
		}
	}

	r.log.Printf("Cache miss for logical chunk %d (location %d), requesting from Telegram API.", cacheChunkID, locationID)

	req := &tg.UploadGetFileRequest{
		Offset:   offset,
		Limit:    int(limit), // Limit sent to Telegram API, which will now always be a valid power-of-2 multiple.
		Location: r.location,
	}
	return r.downloadAndCacheChunk(req, cacheChunkID)
}

// downloadAndCacheChunk handles rate limiting and exponential backoff.
func (r *telegramReader) downloadAndCacheChunk(req *tg.UploadGetFileRequest, cacheChunkID int64) ([]byte, error) {
	delay := baseDelay

	var locationID int64
	switch l := req.Location.(type) {
	case *tg.InputDocumentFileLocation:
		locationID = l.ID
	case *tg.InputPhotoFileLocation:
		locationID = l.ID
	default:
		return nil, fmt.Errorf("unsupported location type for caching in downloadAndCacheChunk: %T", req.Location)
	}

	for retryCount := 0; retryCount < maxRetries; retryCount++ {
		mu.Lock()
		<-rateLimiter.C
		mu.Unlock()

		// DEBUG: Log the actual offset and limit being sent to Telegram
		r.log.Printf("DEBUG: Sending UploadGetFileRequest for chunk %d (location %d): Offset=%d, Limit=%d, LocationType=%T",
			cacheChunkID, locationID, req.Offset, req.Limit, req.Location)

		res, err := r.client.API().UploadGetFile(r.ctx, req)
		if err != nil {
			if floodWait, ok := isFloodWaitError(err); ok {
				r.log.Printf("FLOOD_WAIT error: retrying in %d seconds.", floodWait)
				time.Sleep(time.Duration(floodWait) * time.Second)
				continue
			}

			if isTransientError(err) {
				r.log.Printf("Transient error: %v, retrying in %v", err, delay)
				time.Sleep(delay)
				delay = minDuration(delay*2, maxDelay)
				continue
			}

			r.log.Printf("Error during chunk download: %v", err)
			return nil, err
		}

		switch result := res.(type) {
		case *tg.UploadFile:
			chunkData := result.Bytes
			// Write to cache using the `cacheChunkID` derived from the logical chunk.
			// The `BinaryCache.writeChunk` must be able to handle `chunkData`
			// that is smaller than its `fixedChunkSize` (preferredChunkSize).
			// It currently does this correctly by padding, or storing exact size.
			// When reading, it reconstructs the original `chunkData`.
			err = r.cache.writeChunk(locationID, cacheChunkID, chunkData)
			if err != nil {
				r.log.Printf("Error writing chunk to cache (location %d, chunk %d): %v", locationID, cacheChunkID, err)
			}
			return chunkData, nil
		default:
			return nil, fmt.Errorf("Unexpected response type: %T", result)
		}
	}

	return nil, fmt.Errorf("failed to download chunk %d for location %d after %d retries", cacheChunkID, locationID, maxRetries)
}

// partStream returns a function that fetches raw file chunks from the Telegram API.
func (r *telegramReader) partStream() func() ([]byte, error) {
	// currentAPIOffset is the offset at which Telegram API requests will start.
	// It must be a multiple of r.chunkSize (preferredChunkSize) to align with our caching strategy.
	// The initial offset from r.start is aligned to the nearest r.chunkSize boundary below it.
	currentAPIOffset := r.start - (r.start % r.chunkSize)

	return func() ([]byte, error) {
		if currentAPIOffset >= r.contentLength {
			return nil, io.EOF
		}

		// The limit for the Telegram API request. We consistently use preferredChunkSize.
		// Telegram API documentation and observed behavior for robust clients implies
		// that asking for a larger chunk than remaining data is handled by returning fewer bytes,
		// not a LIMIT_INVALID error, as long as the requested limit itself is valid (e.g., 4096 * 2^N).
		limitToRequest := r.chunkSize // This is preferredChunkSize (256KB), which is 4096 * 2^6

		// Ensure limit does not exceed Telegram's absolute maximum.
		if limitToRequest > telegramMaxLimit {
			limitToRequest = telegramMaxLimit
		}

		r.log.Printf("DEBUG: Requesting chunk: Offset=%d, Limit=%d (using fixed preferredChunkSize)",
			currentAPIOffset, limitToRequest)

		// Make the API request.
		chunkData, err := r.chunk(currentAPIOffset, limitToRequest)
		if err != nil {
			r.log.Printf("Error fetching chunk from Telegram API for offset %d, limit %d: %v", currentAPIOffset, limitToRequest, err)
			return nil, err
		}

		if len(chunkData) == 0 && (r.contentLength-currentAPIOffset) > 0 {
			// If we get an empty chunk but expected more, it's an issue.
			r.log.Printf("Received empty chunk from Telegram API for offset %d, limit %d, but expected more bytes (remaining: %d). Treating as EOF.", currentAPIOffset, limitToRequest, (r.contentLength - currentAPIOffset))
			return nil, io.EOF
		}

		// Advance the API offset by `r.chunkSize` (preferredChunkSize).
		// This maintains alignment for subsequent requests and cache indexing.
		// The `Read` method's `r.buffer` and `r.i` logic will handle the actual
		// bytes consumed from this potentially larger (or truncated at EOF) chunkData.
		currentAPIOffset += r.chunkSize

		return chunkData, nil
	}
}

// isFloodWaitError checks if the error is a FLOOD_WAIT error.
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

// isTransientError checks if an error is transient.
func isTransientError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}

	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
		return 0
	}
	minVal := vals[0]
	for _, v := range vals {
		if v < minVal {
			minVal = v
		}
	}
	return minVal
}
