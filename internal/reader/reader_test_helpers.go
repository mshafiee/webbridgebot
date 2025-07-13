package reader

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/mock"
)

// --- Mocks & Test Setup ---

// mockInvoker is a mock of the bin.Invoker interface, which is the lowest level of API call execution.
type mockInvoker struct {
	mock.Mock
}

// Invoke mocks the RPC call. It checks the input type and provides a canned response.
func (m *mockInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	// The `Called` method tracks the call and returns the configured values.
	args := m.Called(ctx, input, output)
	err := args.Error(1)
	if err != nil {
		return err
	}

	// If a response object is configured, encode it into the `output` decoder.
	if resp := args.Get(0); resp != nil {
		if respEncoder, ok := resp.(bin.Encoder); ok {
			var b bin.Buffer
			if err := respEncoder.Encode(&b); err != nil {
				return err
			}
			// The output is a pointer to the expected result struct, so we decode into it.
			return output.Decode(&b)
		}
	}
	return nil
}

// mockTGClient is a mock that implements the telegramClient interface from reader.go.
type mockTGClient struct {
	api *tg.Client
}

func (m *mockTGClient) API() *tg.Client {
	return m.api
}

// generateTestData creates a byte slice of a given size with predictable content.
func generateTestData(size int) []byte {
	data := make([]byte, size)
	for i := 0; i < size; i++ {
		data[i] = byte(i % 256)
	}
	return data
}

// setupTestCache is a helper function to create a new BinaryCache for tests.
func setupTestCache(t *testing.T, maxCacheSize, fixedChunkSize int64) (*BinaryCache, string) {
	tempDir := t.TempDir()
	cache, err := NewBinaryCache(tempDir, maxCacheSize, fixedChunkSize)
	if err != nil {
		t.Fatalf("Failed to initialize BinaryCache: %v", err)
	}
	return cache, tempDir
}

// closeCache is a helper to ensure the cache is closed gracefully.
func closeCache(t *testing.T, cache *BinaryCache) {
	if err := cache.Close(); err != nil {
		t.Errorf("Error closing cache: %v", err)
	}
}
