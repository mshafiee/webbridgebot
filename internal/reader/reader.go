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
	chunkSize            = int64(1024 * 1024)
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
	location      *tg.InputDocumentFileLocation
	start         int64
	end           int64
	next          func() ([]byte, error)
	buffer        []byte
	bytesread     int64
	chunkSize     int64
	i             int64
	contentLength int64
	cache         *BinaryCache
}

// NewTelegramReader initializes a new telegramReader with the given parameters, including a BinaryCache.
func NewTelegramReader(ctx context.Context, client *gotgproto.Client, location *tg.InputDocumentFileLocation, start int64, end int64, contentLength int64, cache *BinaryCache, logger *log.Logger) (io.ReadCloser, error) {
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
	r.log.Println("Initialization complete.")
	r.next = r.partStream()
	return r, nil
}

// Close implements the io.Closer interface but doesn't perform any actions.
func (*telegramReader) Close() error {
	return nil
}

// Read reads the next chunk of data into the provided byte slice.
func (r *telegramReader) Read(p []byte) (n int, err error) {

	if r.bytesread == r.contentLength {
		r.log.Println("Reached end of cacheFile (bytesread == contentLength).")
		return 0, io.EOF
	}

	if r.i >= int64(len(r.buffer)) {
		r.buffer, err = r.next()
		if err != nil {
			r.log.Printf("Error while reading data: %v", err)
			return 0, err
		}
		if len(r.buffer) == 0 {
			r.next = r.partStream()
			r.buffer, err = r.next()
			if err != nil {
				r.log.Printf("Error while reading data: %v", err)
				return 0, err
			}
		}
		r.i = 0
	}
	n = copy(p, r.buffer[r.i:])
	r.i += int64(n)
	r.bytesread += int64(n)
	return n, nil
}

// chunk requests a cacheFile chunk from the Telegram API starting at the specified offset or retrieves it from the cache.
func (r *telegramReader) chunk(offset int64, limit int64) ([]byte, error) {
	// Check if the chunk is already in the cache
	chunkID := offset / r.chunkSize
	cachedChunk, err := r.cache.readChunk(r.location.ID, chunkID)
	if err == nil {
		r.log.Printf("Cache hit for chunk %d.", chunkID)
		return cachedChunk, nil
	}

	r.log.Printf("Cache miss for chunk %d, requesting from Telegram API.", chunkID)

	// If not in cache, request it from Telegram
	req := &tg.UploadGetFileRequest{
		Offset:   offset,
		Limit:    int(limit),
		Location: r.location,
	}
	return r.downloadAndCacheChunk(req, chunkID)
}

// downloadAndCacheChunk combines rate limiting and exponential backoff.
func (r *telegramReader) downloadAndCacheChunk(req *tg.UploadGetFileRequest, chunkID int64) ([]byte, error) {
	delay := baseDelay // Start with the base delay for exponential backoff.

	for retryCount := 0; retryCount < maxRetries; retryCount++ {
		// Rate limiting: Wait for the rate limiter to allow a new request.
		mu.Lock()
		<-rateLimiter.C
		mu.Unlock()

		res, err := r.client.API().UploadGetFile(r.ctx, req)
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
				delay = min(delay*2, maxDelay) // Increase delay with exponential backoff, capping at maxDelay.
				continue
			}

			// Return non-transient errors without retrying.
			r.log.Printf("Error during chunk download: %v", err)
			return nil, err
		}

		switch result := res.(type) {
		case *tg.UploadFile:
			chunkData := result.Bytes
			err = r.cache.writeChunk(r.location.ID, chunkID, chunkData)
			if err != nil {
				r.log.Printf("Error writing chunk to cache: %v", err)
			}
			return chunkData, nil
		default:
			return nil, fmt.Errorf("Unexpected response type: %T", r)
		}
	}

	// If all retries are exhausted, return an error.
	return nil, fmt.Errorf("failed to download chunk %d after %d retries", chunkID, maxRetries)
}

// partStream returns a function that reads cacheFile chunks sequentially.
func (r *telegramReader) partStream() func() ([]byte, error) {
	start := r.start
	end := r.end
	offset := start - (start % r.chunkSize)

	firstPartCut := start - offset
	lastPartCut := (end % r.chunkSize) + 1
	partCount := int((end - offset + r.chunkSize) / r.chunkSize)
	currentPart := 1

	readData := func() ([]byte, error) {
		if currentPart > partCount {
			return make([]byte, 0), nil
		}
		res, err := r.chunk(offset, r.chunkSize)
		if err != nil {
			return nil, err
		}
		if len(res) == 0 {
			return res, nil
		} else if partCount == 1 {
			res = res[firstPartCut:lastPartCut]
		} else if currentPart == 1 {
			res = res[firstPartCut:]
		} else if currentPart == partCount {
			res = res[:lastPartCut]
		}

		currentPart++
		offset += r.chunkSize
		return res, nil
	}
	return readData
}

// isFloodWaitError checks if the error is a FLOOD_WAIT error and returns the wait time if true.
func isFloodWaitError(err error) (int, bool) {
	// Identify FLOOD_WAIT errors and extract wait time if applicable.
	errText := err.Error()
	matched, _ := regexp.MatchString(`FLOOD_WAIT \(\d+\)`, errText)
	if matched {
		// Extract the wait time in seconds using a regular expression.
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
	// Handle network-related errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		// Retry on network timeouts or temporary errors
		return netErr.Timeout() || netErr.Temporary()
	}

	// Check for specific system call errors that might indicate a transient issue
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}

	// Handle context cancellation or deadline exceeded errors
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// These are transient in the sense that they may succeed if retried, depending on the situation
		return true
	}

	// If none of the above conditions match, consider the error non-transient
	return false
}

// min returns the minimum of two time.Duration values.
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
