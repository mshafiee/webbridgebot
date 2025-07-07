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
	// It must be a multiple of apiAlignment.
	// To avoid potential 'LIMIT_INVALID' errors that some Telegram MTProto servers exhibit
	// when 'limit' is not `apiAlignment * (power of 2)`, we pick a power-of-2 multiple.
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

	if r.i >= int64(len(r.buffer)) {
		r.buffer, err = r.next()
		if err != nil {
			return 0, err
		}
		r.i = 0

		if r.bytesread == 0 {
			// Calculate the initial offset into the first received chunk.
			// This initialAlignedOffset is guaranteed to be a multiple of r.chunkSize,
			// and thus a multiple of apiAlignment (4096), which is required by Telegram.
			initialAlignedOffset := r.start - (r.start % r.chunkSize)
			bytesToSkip := r.start - initialAlignedOffset

			if bytesToSkip < int64(len(r.buffer)) {
				r.buffer = r.buffer[bytesToSkip:]
			} else {
				r.log.Printf("Read: Bytes to skip (%d) for initial offset (%d) exceeds first buffer length (%d). Likely EOF or invalid range.", bytesToSkip, r.start, len(r.buffer))
				return 0, io.EOF // This might happen if 'start' is at or beyond contentLength
			}
		}
	}

	bytesLeftInBuffer := int64(len(r.buffer)) - r.i
	bytesRemainingForRequest := totalBytesToServe - r.bytesread

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

	chunkID := offset / r.chunkSize // Use r.chunkSize (preferredChunkSize) for cache chunk ID
	cachedChunk, err := r.cache.readChunk(locationID, chunkID)
	if err == nil {
		r.log.Printf("Cache hit for chunk %d (location %d).", chunkID, locationID)
		return cachedChunk, nil
	}

	r.log.Printf("Cache miss for chunk %d (location %d), requesting from Telegram API.", chunkID, locationID)

	req := &tg.UploadGetFileRequest{
		Offset:   offset,
		Limit:    int(limit), // Limit sent to Telegram API
		Location: r.location,
	}
	return r.downloadAndCacheChunk(req, chunkID)
}

// downloadAndCacheChunk handles rate limiting and exponential backoff.
func (r *telegramReader) downloadAndCacheChunk(req *tg.UploadGetFileRequest, chunkID int64) ([]byte, error) {
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
			chunkID, locationID, req.Offset, req.Limit, req.Location)

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
			// Write to cache using the preferredChunkSize for the part metadata.
			// The actual data length might be less than 'limit' if it's the last chunk.
			err = r.cache.writeChunk(locationID, chunkID, chunkData)
			if err != nil {
				r.log.Printf("Error writing chunk to cache (location %d, chunk %d): %v", locationID, chunkID, err)
			}
			return chunkData, nil
		default:
			return nil, fmt.Errorf("Unexpected response type: %T", result)
		}
	}

	return nil, fmt.Errorf("failed to download chunk %d for location %d after %d retries", chunkID, locationID, maxRetries)
}

// partStream returns a function that fetches raw file chunks from the Telegram API.
func (r *telegramReader) partStream() func() ([]byte, error) {
	// currentAlignedFileOffset is the offset at which Telegram API requests will start.
	// It must be a multiple of r.chunkSize (preferredChunkSize), and thus a multiple of apiAlignment (4096).
	currentAlignedFileOffset := r.start - (r.start % r.chunkSize)

	return func() ([]byte, error) {
		if currentAlignedFileOffset >= r.contentLength {
			return nil, io.EOF
		}

		// Calculate the number of bytes remaining from the current aligned offset
		// to the end of the file.
		remainingBytesInFile := r.contentLength - currentAlignedFileOffset

		// Determine the base limit for this request. It should be the smaller of
		// preferredChunkSize (our desired chunk size) and the remaining bytes.
		limit := preferredChunkSize // Our preferred chunk size
		if remainingBytesInFile < preferredChunkSize {
			limit = remainingBytesInFile
		}

		// Ensure the limit is rounded UP to the nearest multiple of apiAlignment (4096).
		// This formula handles both cases:
		// 1. If 'limit' is already a multiple, it remains unchanged.
		// 2. If 'limit' is not a multiple, it's rounded up to the next one.
		if limit > 0 { // Only apply rounding if limit is positive
			limit = ((limit + apiAlignment - 1) / apiAlignment) * apiAlignment
		} else { // If limit is 0 or negative, it's an EOF or invalid state, caught by earlier checks.
			return nil, io.EOF
		}

		// After rounding up, ensure the limit does not exceed Telegram's absolute maximum.
		if limit > telegramMaxLimit {
			limit = telegramMaxLimit
		}

		// If after all calculations, limit somehow became zero or negative, treat as EOF.
		if limit <= 0 {
			return nil, io.EOF
		}

		// Log the actual offset and limit being sent to Telegram for debugging
		r.log.Printf("DEBUG: Requesting chunk: Offset=%d, Limit=%d (calculated from remaining=%d, preferredChunkSize=%d, pre-rounded_limit=%d)",
			currentAlignedFileOffset, limit, remainingBytesInFile, preferredChunkSize, remainingBytesInFile)

		chunkData, err := r.chunk(currentAlignedFileOffset, limit)
		if err != nil {
			return nil, err
		}

		if len(chunkData) == 0 {
			r.log.Printf("Received empty chunk from Telegram API for offset %d, limit %d, but content length is %d. Treating as EOF.", currentAlignedFileOffset, limit, r.contentLength)
			return nil, io.EOF
		}

		// Advance the aligned offset by r.chunkSize (preferredChunkSize) for the next iteration.
		// This aligns with our internal chunking strategy for caching and ensures consistent
		// progression through the file based on our preferred chunk size.
		currentAlignedFileOffset += r.chunkSize

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
