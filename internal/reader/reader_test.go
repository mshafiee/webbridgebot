package reader

import (
	"context"
	"fmt"
	"io"
	"log"
	"syscall"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestTelegramReader_ReadLogic verifies the data assembly logic of the Read method,
// including handling of unaligned starts and ends, by mocking the chunk fetching layer.
func TestTelegramReader_ReadLogic(t *testing.T) {
	fullTestData := generateTestData(int(preferredChunkSize * 3))
	contentLength := int64(len(fullTestData))

	// testHarness creates a telegramReader with a mocked chunk fetching function.
	testHarness := func(t *testing.T, start, end, contentLength int64, mockChunker func(offset, limit int64) ([]byte, error)) io.ReadCloser {
		r := &telegramReader{
			ctx:           context.Background(),
			log:           log.New(io.Discard, "", 0),
			location:      &tg.InputDocumentFileLocation{ID: 12345},
			start:         start,
			end:           end,
			chunkSize:     preferredChunkSize,
			contentLength: contentLength,
		}

		// This partStream is a copy of the original but calls our mockChunker.
		partStreamTest := func() func() ([]byte, error) {
			currentAPIOffset := r.start - (r.start % r.chunkSize)
			return func() ([]byte, error) {
				if currentAPIOffset >= r.contentLength {
					return nil, io.EOF
				}
				limitToRequest := r.chunkSize
				if limitToRequest > telegramMaxLimit {
					limitToRequest = telegramMaxLimit
				}

				chunkData, err := mockChunker(currentAPIOffset, limitToRequest)
				if err != nil {
					return nil, err
				}
				if len(chunkData) == 0 && (r.contentLength-currentAPIOffset) > 0 {
					return nil, io.EOF
				}
				currentAPIOffset += r.chunkSize
				return chunkData, nil
			}
		}

		r.next = partStreamTest()
		return r
	}

	t.Run("Full Read Aligned", func(t *testing.T) {
		start, end := int64(0), contentLength-1
		mockChunker := func(offset, limit int64) ([]byte, error) {
			chunkEnd := offset + limit
			if chunkEnd > contentLength {
				chunkEnd = contentLength
			}
			return fullTestData[offset:chunkEnd], nil
		}
		reader := testHarness(t, start, end, contentLength, mockChunker)

		readBytes, err := io.ReadAll(reader)
		assert.NoError(t, err)
		assert.Equal(t, fullTestData, readBytes)
	})

	t.Run("Read with Unaligned Start and End", func(t *testing.T) {
		start, end := int64(100), int64(600) // Spans multiple chunks
		expectedData := fullTestData[start : end+1]
		mockChunker := func(offset, limit int64) ([]byte, error) {
			assert.True(t, offset%preferredChunkSize == 0, "offset should be aligned")
			chunkEnd := offset + limit
			if chunkEnd > contentLength {
				chunkEnd = contentLength
			}
			return fullTestData[offset:chunkEnd], nil
		}
		reader := testHarness(t, start, end, contentLength, mockChunker)

		readBytes, err := io.ReadAll(reader)
		assert.NoError(t, err)
		assert.Equal(t, expectedData, readBytes)
		assert.Equal(t, len(expectedData), len(readBytes))
	})
}

// newTestReader creates a telegramReader instance with a mocked API client.
// This is the primary helper for integration-style tests.
func newTestReader(t *testing.T, client telegramClient, location tg.InputFileLocationClass, cache *BinaryCache, start, end, contentLength int64) *telegramReader {
	r := &telegramReader{
		ctx:           context.Background(),
		log:           log.New(io.Discard, "", 0),
		location:      location,
		client:        client,
		start:         start,
		end:           end,
		chunkSize:     preferredChunkSize,
		contentLength: contentLength,
		cache:         cache,
	}
	r.next = r.partStream()
	return r
}

// TestTelegramReader_CachingAndRetries tests the integration with the cache and the API retry logic.
func TestTelegramReader_CachingAndRetries(t *testing.T) {
	// Speed up tests by reducing retry delays.
	oldRateLimiter := rateLimiter
	oldBaseDelay := baseDelay
	rateLimiter = time.NewTicker(time.Microsecond)
	baseDelay = time.Microsecond
	t.Cleanup(func() {
		rateLimiter.Stop()
		rateLimiter = oldRateLimiter
		baseDelay = oldBaseDelay
	})

	testData := generateTestData(int(preferredChunkSize))
	contentLength := int64(len(testData))
	location := &tg.InputDocumentFileLocation{ID: 12345}

	t.Run("Cache Miss then Cache Hit", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		// --- First Read (Cache Miss) ---
		// Expect one call to the API.
		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData}
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(apiResponse, nil).Once()

		reader1 := newTestReader(t, client, location, cache, 0, contentLength-1, contentLength)

		readBytes1, err := io.ReadAll(reader1)
		assert.NoError(t, err)
		assert.Equal(t, testData, readBytes1)
		invoker.AssertExpectations(t) // Verify the API was called.

		// --- Second Read (Cache Hit) ---
		// The mock is configured to only be called once. If it's called again, the test will fail.
		reader2 := newTestReader(t, client, location, cache, 0, contentLength-1, contentLength)

		readBytes2, err := io.ReadAll(reader2)
		assert.NoError(t, err)
		assert.Equal(t, testData, readBytes2)
		invoker.AssertExpectations(t) // Verify no new API calls were made.
	})

	t.Run("Retries on Transient Error", func(t *testing.T) {
		invoker := new(mockInvoker)
		mockedTgClient := tg.NewClient(invoker)
		client := &mockTGClient{api: mockedTgClient}

		cache, _ := setupTestCache(t, 1024*1024, preferredChunkSize)
		defer closeCache(t, cache)

		// Setup mock to fail twice with a transient error, then succeed.
		transientErr := syscall.ECONNRESET
		apiResponse := &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: testData}
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(nil, transientErr).Twice()
		invoker.On("Invoke", mock.Anything, mock.Anything, mock.Anything).Return(apiResponse, nil).Once()

		reader := newTestReader(t, client, location, cache, 0, contentLength-1, contentLength)

		readBytes, err := io.ReadAll(reader)
		assert.NoError(t, err)
		assert.Equal(t, testData, readBytes)
		invoker.AssertExpectations(t) // Verify it was called 3 times in total.
	})
}

// TestTelegramReader_Helpers provides simple unit tests for the utility functions.
func TestTelegramReader_Helpers(t *testing.T) {
	t.Run("isFloodWaitError", func(t *testing.T) {
		err := fmt.Errorf("rpc error code 420: FLOOD_WAIT (32)")
		waitTime, ok := isFloodWaitError(err)
		assert.True(t, ok)
		assert.Equal(t, 32, waitTime)

		err = fmt.Errorf("some other error")
		_, ok = isFloodWaitError(err)
		assert.False(t, ok)
	})

	t.Run("isTransientError", func(t *testing.T) {
		assert.True(t, isTransientError(syscall.ECONNRESET))
		assert.True(t, isTransientError(context.Canceled))
		assert.False(t, isTransientError(fmt.Errorf("permanent error")))
	})

	t.Run("minInt64", func(t *testing.T) {
		assert.Equal(t, int64(1), minInt64(5, 1, 3))
		assert.Equal(t, int64(-10), minInt64(1, -5, -10))
		assert.Equal(t, int64(0), minInt64())
	})
}
