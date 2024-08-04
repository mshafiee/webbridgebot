package reader

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestNewBinaryCache(t *testing.T) {
	// Create a temporary directory for the test
	tempDir := t.TempDir()

	// Initialize a new BinaryCache with a max cache size of 1024 bytes and a fixed chunk size of 256 bytes
	cache, err := NewBinaryCache(tempDir, 1024, 256)
	if err != nil {
		t.Fatalf("Failed to initialize BinaryCache: %v", err)
	}

	// Check if cache files exist
	cacheFile := filepath.Join(tempDir, "cache.dat")
	metadataFile := filepath.Join(tempDir, "metadata.dat")

	if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
		t.Errorf("Cache file was not created")
	}

	if _, err := os.Stat(metadataFile); os.IsNotExist(err) {
		t.Errorf("Metadata file was not created")
	}

	// Close the cache files
	cache.cashFile.Close()
	cache.metadataFile.Close()
}

func TestBinaryCache_WriteReadChunk(t *testing.T) {
	// Create a temporary directory for the test
	tempDir := t.TempDir()

	// Initialize a new BinaryCache
	cache, err := NewBinaryCache(tempDir, 1024, 256)
	if err != nil {
		t.Fatalf("Failed to initialize BinaryCache: %v", err)
	}

	locationID := int64(1)
	chunkID := int64(1)
	data := []byte("This is a test chunk of data.")

	// Write the chunk
	err = cache.writeChunk(locationID, chunkID, data)
	if err != nil {
		t.Fatalf("Failed to write chunk: %v", err)
	}

	// Read the chunk back
	readData, err := cache.readChunk(locationID, chunkID)
	if err != nil {
		t.Fatalf("Failed to read chunk: %v", err)
	}

	// Compare the written data with the read data
	if !bytes.Equal(data, readData) {
		t.Errorf("Data mismatch: expected %v, got %v", data, readData)
	}

	// Close the cache files
	cache.cashFile.Close()
	cache.metadataFile.Close()
}

func TestBinaryCache_LRU_Eviction(t *testing.T) {
	// Create a temporary directory for the test
	tempDir := t.TempDir()

	// Initialize a new BinaryCache with a small max cache size to force eviction
	cache, err := NewBinaryCache(tempDir, 512, 256)
	if err != nil {
		t.Fatalf("Failed to initialize BinaryCache: %v", err)
	}

	locationID := int64(1)
	data1 := make([]byte, 256) // 256 bytes
	data2 := make([]byte, 256) // 256 bytes
	data3 := make([]byte, 256) // 256 bytes

	// Write multiple chunks to the cache
	err = cache.writeChunk(locationID, 1, data1)
	if err != nil {
		t.Fatalf("Failed to write chunk 1: %v", err)
	}

	fmt.Println("After writing chunk 1")

	err = cache.writeChunk(locationID, 2, data2)
	if err != nil {
		t.Fatalf("Failed to write chunk 2: %v", err)
	}

	fmt.Println("After writing chunk 2")

	err = cache.writeChunk(locationID, 3, data3) // This should trigger eviction
	if err != nil {
		t.Fatalf("Failed to write chunk 3: %v", err)
	}

	fmt.Println("After writing chunk 3 and before checking eviction")

	// Check that chunk 1 was evicted (since cache size is limited)
	_, err = cache.readChunk(locationID, 1)
	if err == nil {
		t.Error("Expected chunk 1 to be evicted, but it was not")
	} else {
		fmt.Printf("Chunk 1 was successfully evicted, received error: %v\n", err)
	}

	// Check that chunk 2 and chunk 3 are still present
	_, err = cache.readChunk(locationID, 2)
	if err != nil {
		t.Errorf("Chunk 2 should still be present, but got error: %v", err)
	}

	_, err = cache.readChunk(locationID, 3)
	if err != nil {
		t.Errorf("Chunk 3 should still be present, but got error: %v", err)
	}

	// Close the cache files
	cache.cashFile.Close()
	cache.metadataFile.Close()
}

func TestBinaryCache_MetadataPersistence(t *testing.T) {
	// Create a temporary directory for the test
	tempDir := t.TempDir()

	// Initialize a new BinaryCache
	cache, err := NewBinaryCache(tempDir, 1024, 256)
	if err != nil {
		t.Fatalf("Failed to initialize BinaryCache: %v", err)
	}

	locationID := int64(1)
	chunkID := int64(1)
	data := []byte("Persistent chunk data.")

	// Write the chunk
	err = cache.writeChunk(locationID, chunkID, data)
	if err != nil {
		t.Fatalf("Failed to write chunk: %v", err)
	}

	// Close and re-open the cache to simulate a restart
	cache.cashFile.Close()
	cache.metadataFile.Close()

	cache, err = NewBinaryCache(tempDir, 1024, 256)
	if err != nil {
		t.Fatalf("Failed to reinitialize BinaryCache: %v", err)
	}

	// Read the chunk back
	readData, err := cache.readChunk(locationID, chunkID)
	if err != nil {
		t.Fatalf("Failed to read chunk after reopening cache: %v", err)
	}

	// Compare the written data with the read data
	if !bytes.Equal(data, readData) {
		t.Errorf("Data mismatch after reopening cache: expected %v, got %v", data, readData)
	}

	// Close the cache files
	cache.cashFile.Close()
	cache.metadataFile.Close()
}

func TestSplitChunk(t *testing.T) {
	// Initialize a BinaryCache with a fixed chunk size
	cache := &BinaryCache{
		fixedChunkSize: 256, // 256 bytes per chunk part
	}

	t.Run("Basic Split", func(t *testing.T) {
		chunk := make([]byte, 512) // 512 bytes, should be split into two parts
		parts := cache.splitChunk(chunk)

		if len(parts) != 2 {
			t.Errorf("Expected 2 parts, got %d", len(parts))
		}

		for i, part := range parts {
			if len(part) != 256 {
				t.Errorf("Part %d expected to have length 256, got %d", i, len(part))
			}
		}
	})

	t.Run("Exact Size Split", func(t *testing.T) {
		chunk := make([]byte, 256) // 256 bytes, should return one part
		parts := cache.splitChunk(chunk)

		if len(parts) != 1 {
			t.Errorf("Expected 1 part, got %d", len(parts))
		}

		if len(parts[0]) != 256 {
			t.Errorf("Expected part to have length 256, got %d", len(parts[0]))
		}
	})

	t.Run("Smaller Chunk", func(t *testing.T) {
		chunk := make([]byte, 100) // 100 bytes, should return one part
		parts := cache.splitChunk(chunk)

		if len(parts) != 1 {
			t.Errorf("Expected 1 part, got %d", len(parts))
		}

		if len(parts[0]) != 100 {
			t.Errorf("Expected part to have length 100, got %d", len(parts[0]))
		}
	})

	t.Run("Empty Chunk", func(t *testing.T) {
		chunk := make([]byte, 0) // Empty chunk, should return no parts
		parts := cache.splitChunk(chunk)

		if len(parts) != 0 {
			t.Errorf("Expected 0 parts, got %d", len(parts))
		}
	})
}
