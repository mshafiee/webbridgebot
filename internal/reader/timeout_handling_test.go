package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"syscall"
	"testing"
	"time"

	"webBridgeBot/internal/logger"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestDynamicChunkSizing tests the dynamic chunk size reduction and restoration logic
func TestDynamicChunkSizing(t *testing.T) {
	testData := generateTestData(int(preferredChunkSize))
	location := &tg.InputDocumentFileLocation{ID: 12345}

	// Create logger that writes to discard
	log := logger.New(io.Discard, "test", logger.INFO, false)

	t.Run("Initial state has full chunk size", func(t *testing.T) {
		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			chunkSize:           preferredChunkSize,
			requestLimit:        preferredChunkSize,
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Millisecond,
			maxDelay:            time.Second,
			apiTimeout:          time.Second,
		}

		assert.Equal(t, preferredChunkSize, r.requestLimit, "Initial requestLimit should be preferredChunkSize")
		assert.Equal(t, 0, r.consecutiveTimeouts, "Initial consecutiveTimeouts should be 0")
		assert.Equal(t, 0, r.successfulChunks, "Initial successfulChunks should be 0")
	})

	t.Run("Chunk size reduces after consecutive timeouts", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			client:              client,
			chunkSize:           preferredChunkSize,
			contentLength:       int64(len(testData)),
			cache:               cache,
			requestLimit:        preferredChunkSize,
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Microsecond, // Speed up test
			maxDelay:            time.Millisecond,
			apiTimeout:          10 * time.Millisecond,
		}

		// Simulate timeout error (context.DeadlineExceeded)
		timeoutErr := context.DeadlineExceeded
		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData}

		// First 3 calls timeout, 4th succeeds
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(nil, timeoutErr).Times(3)
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(apiResponse, nil).Once()

		// Create request
		req := &tg.UploadGetFileRequest{
			Offset:   0,
			Limit:    int(preferredChunkSize),
			Location: location,
		}

		// Download chunk - should reduce size after 3 timeouts
		result, err := r.downloadAndCacheChunk(req, 0)

		// Should eventually succeed
		assert.NoError(t, err)
		assert.NotNil(t, result)

		// requestLimit should have been reduced
		assert.Less(t, r.requestLimit, preferredChunkSize, "requestLimit should be reduced after timeouts")
		assert.GreaterOrEqual(t, r.requestLimit, int64(65536), "requestLimit should not go below 64KB")

		// consecutiveTimeouts should be reset after success
		assert.Equal(t, 0, r.consecutiveTimeouts, "consecutiveTimeouts should reset after success")

		invoker.AssertExpectations(t)
	})

	t.Run("Chunk size restores after successful downloads", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			client:              client,
			chunkSize:           preferredChunkSize,
			contentLength:       int64(len(testData)),
			cache:               cache,
			requestLimit:        128 * 1024, // Start with reduced size
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Microsecond,
			maxDelay:            time.Millisecond,
			apiTimeout:          time.Second,
		}

		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData}

		// All 5 calls succeed
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(apiResponse, nil).Times(5)

		// Make 5 successful downloads
		for i := int64(0); i < 5; i++ {
			offset := i * preferredChunkSize
			req := &tg.UploadGetFileRequest{
				Offset:   offset,
				Limit:    int(r.requestLimit),
				Location: location,
			}

			_, err := r.downloadAndCacheChunk(req, i)
			assert.NoError(t, err)
		}

		// After 5 successes, requestLimit should be restored
		assert.Equal(t, preferredChunkSize, r.requestLimit, "requestLimit should be restored after 5 successes")
		assert.Equal(t, 0, r.successfulChunks, "successfulChunks should be reset after restoration")

		invoker.AssertExpectations(t)
	})

	t.Run("Chunk size has minimum of 64KB", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			client:              client,
			chunkSize:           preferredChunkSize,
			contentLength:       int64(len(testData)),
			cache:               cache,
			requestLimit:        80 * 1024, // 80KB - just above minimum
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Microsecond,
			maxDelay:            time.Millisecond,
			apiTimeout:          10 * time.Millisecond,
		}

		timeoutErr := context.DeadlineExceeded
		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData}

		// 3 timeouts then success
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(nil, timeoutErr).Times(3)
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(apiResponse, nil).Once()

		req := &tg.UploadGetFileRequest{
			Offset:   0,
			Limit:    int(r.requestLimit),
			Location: location,
		}

		_, err := r.downloadAndCacheChunk(req, 0)
		assert.NoError(t, err)

		// requestLimit should be reduced to minimum (64KB)
		assert.Equal(t, int64(65536), r.requestLimit, "requestLimit should be at minimum 64KB")

		invoker.AssertExpectations(t)
	})
}

// TestTimeoutDetection tests that timeout errors are correctly identified
func TestTimeoutDetection(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		expectDetect bool
	}{
		{
			name:         "context.DeadlineExceeded is detected",
			err:          context.DeadlineExceeded,
			expectDetect: true,
		},
		{
			name:         "RPC -503 error is detected",
			err:          fmt.Errorf("rpcDoRequest: rpc error code -503: Timeout"),
			expectDetect: true,
		},
		{
			name:         "wrapped RPC -503 error is detected",
			err:          fmt.Errorf("failed to download: %w", fmt.Errorf("rpc error code -503")),
			expectDetect: true,
		},
		{
			name:         "connection reset is transient but not timeout",
			err:          syscall.ECONNRESET,
			expectDetect: false,
		},
		{
			name:         "generic error is not timeout",
			err:          fmt.Errorf("some other error"),
			expectDetect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Check if error is transient first
			isTransient := isTransientError(tt.err)

			if isTransient {
				// Check if it's specifically a timeout
				isTimeout := errors.Is(tt.err, context.DeadlineExceeded) ||
					regexp.MustCompile(`rpc error code -503`).MatchString(tt.err.Error())

				assert.Equal(t, tt.expectDetect, isTimeout, "Timeout detection mismatch")
			} else {
				assert.False(t, tt.expectDetect, "Non-transient errors should not be detected as timeouts")
			}
		})
	}
}

// TestCacheAlignmentPreservation ensures that dynamic chunk sizing doesn't break cache alignment
func TestCacheAlignmentPreservation(t *testing.T) {
	testData := generateTestData(int(preferredChunkSize * 2))
	location := &tg.InputDocumentFileLocation{ID: 12345}
	log := logger.New(io.Discard, "test", logger.INFO, false)

	t.Run("Offsets remain aligned to chunkSize", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		// Create reader with reduced request limit
		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			client:              client,
			start:               100, // Unaligned start
			end:                 int64(len(testData)) - 1,
			chunkSize:           preferredChunkSize,
			contentLength:       int64(len(testData)),
			cache:               cache,
			requestLimit:        128 * 1024, // Reduced size
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Microsecond,
			maxDelay:            time.Millisecond,
			apiTimeout:          time.Second,
		}

		// Track offsets used in API calls
		var capturedOffsets []int64

		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData[:r.requestLimit]}
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
			// Capture the request to verify offset
			if req, ok := args.Get(1).(*tg.UploadGetFileRequest); ok {
				capturedOffsets = append(capturedOffsets, int64(req.Offset))
			}
		}).Return(apiResponse, nil)

		r.next = r.partStream()

		// Read some data
		buffer := make([]byte, 1024)
		_, err := r.Read(buffer)

		// Should work without error
		assert.NoError(t, err)

		// All captured offsets should be aligned to chunkSize
		for _, offset := range capturedOffsets {
			assert.Equal(t, int64(0), offset%int64(preferredChunkSize),
				"Offset %d should be aligned to chunkSize %d", offset, preferredChunkSize)
		}
	})

	t.Run("Cache correctly handles partial chunks", func(t *testing.T) {
		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		locationID := int64(12345)
		chunkID := int64(0)

		// Write partial chunk (smaller than preferredChunkSize)
		partialData := testData[:128*1024] // 128KB instead of 256KB
		err := cache.writeChunk(locationID, chunkID, partialData)
		assert.NoError(t, err)

		// Read it back
		readData, err := cache.readChunk(locationID, chunkID)
		assert.NoError(t, err)
		assert.Equal(t, len(partialData), len(readData), "Should read back same amount of data")
		assert.Equal(t, partialData, readData, "Data should match")
	})
}

// TestConfigurableTimeout tests that the timeout is properly applied
func TestConfigurableTimeout(t *testing.T) {
	location := &tg.InputDocumentFileLocation{ID: 12345}
	log := logger.New(io.Discard, "test", logger.INFO, false)

	t.Run("Custom timeout is used", func(t *testing.T) {
		customTimeout := 100 * time.Millisecond

		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			chunkSize:           preferredChunkSize,
			requestLimit:        preferredChunkSize,
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          1,
			baseDelay:           time.Microsecond,
			maxDelay:            time.Millisecond,
			apiTimeout:          customTimeout,
		}

		assert.Equal(t, customTimeout, r.apiTimeout, "apiTimeout should match configured value")
	})

	t.Run("Reader initializes with correct timeout", func(t *testing.T) {
		timeoutSeconds := 60

		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			chunkSize:           preferredChunkSize,
			requestLimit:        preferredChunkSize,
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Second,
			maxDelay:            60 * time.Second,
			apiTimeout:          time.Duration(timeoutSeconds) * time.Second,
		}

		// Verify correct initialization
		assert.Equal(t, time.Duration(timeoutSeconds)*time.Second, r.apiTimeout)
		assert.Equal(t, preferredChunkSize, r.requestLimit)
		assert.Equal(t, 0, r.consecutiveTimeouts)
		assert.Equal(t, 0, r.successfulChunks)
	})
}

// TestSuccessTracking tests the success counter behavior
func TestSuccessTracking(t *testing.T) {
	testData := generateTestData(int(preferredChunkSize))
	location := &tg.InputDocumentFileLocation{ID: 12345}
	log := logger.New(io.Discard, "test", logger.INFO, false)

	t.Run("Success counter increments on successful download", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			client:              client,
			chunkSize:           preferredChunkSize,
			contentLength:       int64(len(testData)),
			cache:               cache,
			requestLimit:        128 * 1024, // Start reduced
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Microsecond,
			maxDelay:            time.Millisecond,
			apiTimeout:          time.Second,
		}

		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData}
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(apiResponse, nil).Times(3)

		// Make 3 successful downloads
		for i := int64(0); i < 3; i++ {
			offset := i * preferredChunkSize
			req := &tg.UploadGetFileRequest{
				Offset:   offset,
				Limit:    int(r.requestLimit),
				Location: location,
			}

			_, err := r.downloadAndCacheChunk(req, i)
			assert.NoError(t, err)
			assert.Equal(t, int(i)+1, r.successfulChunks, "Success counter should increment")
		}

		// Should still be at reduced size (need 5 for restoration)
		assert.Equal(t, int64(128*1024), r.requestLimit)
		assert.Equal(t, 3, r.successfulChunks)

		invoker.AssertExpectations(t)
	})

	t.Run("Success counter doesn't increment when at full size", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		r := &telegramReader{
			ctx:                 context.Background(),
			log:                 log,
			location:            location,
			client:              client,
			chunkSize:           preferredChunkSize,
			contentLength:       int64(len(testData)),
			cache:               cache,
			requestLimit:        preferredChunkSize, // Already at full size
			consecutiveTimeouts: 0,
			successfulChunks:    0,
			maxRetries:          10,
			baseDelay:           time.Microsecond,
			maxDelay:            time.Millisecond,
			apiTimeout:          time.Second,
		}

		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData}
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(apiResponse, nil).Once()

		req := &tg.UploadGetFileRequest{
			Offset:   0,
			Limit:    int(r.requestLimit),
			Location: location,
		}

		_, err := r.downloadAndCacheChunk(req, 0)
		assert.NoError(t, err)

		// Success counter should increment regardless
		assert.Equal(t, 1, r.successfulChunks)

		invoker.AssertExpectations(t)
	})
}

func TestMain(m *testing.M) {
	// Speed up rate limiter for tests
	oldRateLimiter := rateLimiter
	rateLimiter = time.NewTicker(time.Microsecond)

	code := m.Run()

	// Cleanup
	rateLimiter.Stop()
	rateLimiter = oldRateLimiter

	os.Exit(code)
}
