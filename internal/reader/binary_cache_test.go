package reader

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// setupTestCache is a helper function to create a new BinaryCache for tests.
func setupTestCache(t *testing.T, maxCacheSize, fixedChunkSize int64) (*BinaryCache, string) {
	tempDir := t.TempDir()
	cache, err := NewBinaryCache(tempDir, maxCacheSize, fixedChunkSize)
	if err != nil {
		t.Fatalf("Failed to initialize BinaryCache: %v", err)
	}
	return cache, tempDir
}

// closeCacheFiles is a helper to ensure cache files are closed.
func closeCacheFiles(t *testing.T, cache *BinaryCache) {
	if cache.cashFile != nil {
		err := cache.cashFile.Close()
		if err != nil {
			t.Logf("Error closing cashFile: %v", err) // Log but don't fail, as it might be closed already
		}
	}
	if cache.metadataFile != nil {
		err := cache.metadataFile.Close()
		if err != nil {
			t.Logf("Error closing metadataFile: %v", err) // Log but don't fail
		}
	}
}

func TestNewBinaryCache(t *testing.T) {
	t.Run("Basic Initialization", func(t *testing.T) {
		cache, tempDir := setupTestCache(t, 1024, 256)
		defer closeCacheFiles(t, cache)

		cacheFile := filepath.Join(tempDir, "cache.dat")
		metadataFile := filepath.Join(tempDir, "metadata.dat")

		if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
			t.Errorf("Cache data file was not created at %s", cacheFile)
		}
		if _, err := os.Stat(metadataFile); os.IsNotExist(err) {
			t.Errorf("Metadata file was not created at %s", metadataFile)
		}

		// Check initial state
		if cache.cacheSize != 0 {
			t.Errorf("Expected initial cacheSize to be 0, got %d", cache.cacheSize)
		}
		if cache.lruQueue.Len() != 0 {
			t.Errorf("Expected initial lruQueue to be empty, got %d items", cache.lruQueue.Len())
		}
		if len(cache.lruMap) != 0 {
			t.Errorf("Expected initial lruMap to be empty, got %d items", len(cache.lruMap))
		}
		if len(cache.metadata) != 0 {
			t.Errorf("Expected initial metadata map to be empty, got %d entries", len(cache.metadata))
		}
	})

	t.Run("Initialization with existing non-empty metadata (simulated)", func(t *testing.T) {
		tempDir := t.TempDir()
		// Manually create a dummy metadata file to simulate a restart
		metadataFile := filepath.Join(tempDir, "metadata.dat")
		f, err := os.OpenFile(metadataFile, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			t.Fatalf("Failed to create dummy metadata file: %v", err)
		}
		// Write a minimal valid header (0 logical chunks)
		err = binary.Write(f, binary.LittleEndian, int64(0))
		f.Close()
		if err != nil {
			t.Fatalf("Failed to write dummy metadata: %v", err)
		}

		cache, err := NewBinaryCache(tempDir, 1024, 256)
		if err != nil {
			t.Fatalf("Failed to initialize BinaryCache with existing metadata: %v", err)
		}
		defer closeCacheFiles(t, cache)

		if cache.cacheSize != 0 {
			t.Errorf("Expected cacheSize to be 0 after loading empty metadata, got %d", cache.cacheSize)
		}
		if cache.lruQueue.Len() != 0 {
			t.Errorf("Expected lruQueue to be empty after loading empty metadata, got %d items", cache.lruQueue.Len())
		}
	})
}

func TestBinaryCache_WriteReadChunk(t *testing.T) {
	t.Run("Basic Write and Read", func(t *testing.T) {
		cache, _ := setupTestCache(t, 1024, 256)
		defer closeCacheFiles(t, cache)

		// Use a simple counter for timestamps in this test
		var counter int64
		cache.setTimestampSource(func() int64 {
			counter++
			return counter
		})

		locationID := int64(1)
		chunkID := int64(1)
		data := []byte("This is a test chunk of data.")

		err := cache.writeChunk(locationID, chunkID, data)
		if err != nil {
			t.Fatalf("Failed to write chunk: %v", err)
		}

		readData, err := cache.readChunk(locationID, chunkID)
		if err != nil {
			t.Fatalf("Failed to read chunk: %v", err)
		}

		if !bytes.Equal(data, readData) {
			t.Errorf("Data mismatch: expected %q, got %q", data, readData)
		}

		if cache.cacheSize != cache.fixedChunkSize { // Should be fixedChunkSize due to padding
			t.Errorf("Expected cache size %d, got %d", cache.fixedChunkSize, cache.cacheSize)
		}
		if cache.lruQueue.Len() != 1 {
			t.Errorf("Expected lruQueue length 1, got %d", cache.lruQueue.Len())
		}
		if len(cache.lruMap) != 1 {
			t.Errorf("Expected lruMap length 1, got %d", len(cache.lruMap))
		}
	})

	t.Run("Write and Read Chunk Smaller Than Fixed Size", func(t *testing.T) {
		cache, _ := setupTestCache(t, 1024, 256)
		defer closeCacheFiles(t, cache)

		var counter int64
		cache.setTimestampSource(func() int64 {
			counter++
			return counter
		})

		locationID := int64(2)
		chunkID := int64(1)
		data := []byte("Small data.") // Smaller than 256 bytes

		err := cache.writeChunk(locationID, chunkID, data)
		if err != nil {
			t.Fatalf("Failed to write small chunk: %v", err)
		}

		readData, err := cache.readChunk(locationID, chunkID)
		if err != nil {
			t.Fatalf("Failed to read small chunk: %v", err)
		}

		if !bytes.Equal(data, readData) {
			t.Errorf("Data mismatch for small chunk: expected %q, got %q", data, readData)
		}

		if cache.cacheSize != cache.fixedChunkSize { // Still stores as fixedChunkSize due to padding
			t.Errorf("Expected cache size %d, got %d", cache.fixedChunkSize, cache.cacheSize)
		}
	})

	t.Run("Write and Read Chunk Larger Than Fixed Size (Multiple Parts)", func(t *testing.T) {
		cache, _ := setupTestCache(t, 1024, 256)
		defer closeCacheFiles(t, cache)

		var counter int64
		cache.setTimestampSource(func() int64 {
			counter++
			return counter
		})

		locationID := int64(3)
		chunkID := int64(1)
		data := make([]byte, 500) // Will split into 2 parts: 256 + 244
		for i := range data {
			data[i] = byte(i % 256)
		}

		err := cache.writeChunk(locationID, chunkID, data)
		if err != nil {
			t.Fatalf("Failed to write large chunk: %v", err)
		}

		readData, err := cache.readChunk(locationID, chunkID)
		if err != nil {
			t.Fatalf("Failed to read large chunk: %v", err)
		}

		if !bytes.Equal(data, readData) {
			t.Errorf("Data mismatch for large chunk: expected %v, got %v", data, readData)
		}

		expectedSize := cache.fixedChunkSize * 2 // Two padded parts
		if cache.cacheSize != expectedSize {
			t.Errorf("Expected cache size %d, got %d", expectedSize, cache.cacheSize)
		}
		if len(cache.metadata[locationID][chunkID]) != 2 {
			t.Errorf("Expected 2 parts for logical chunk, got %d", len(cache.metadata[locationID][chunkID]))
		}
	})
}

func TestBinaryCache_ReadChunk_NotFound(t *testing.T) {
	cache, _ := setupTestCache(t, 1024, 256)
	defer closeCacheFiles(t, cache)

	var counter int64
	cache.setTimestampSource(func() int64 {
		counter++
		return counter
	})

	_, err := cache.readChunk(99, 99)
	if err == nil {
		t.Error("Expected an error when reading non-existent location ID, got nil")
	}
	if err.Error() != "location ID 99 not found" {
		t.Errorf("Expected error 'location ID 99 not found', got %q", err.Error())
	}

	// Write a location but not the specific chunk
	locationID := int64(1)
	chunkID := int64(1)
	data := []byte("Some data")
	err = cache.writeChunk(locationID, chunkID, data)
	if err != nil {
		t.Fatalf("Failed to write initial chunk: %v", err)
	}

	_, err = cache.readChunk(locationID, 99)
	if err == nil {
		t.Error("Expected an error when reading non-existent chunk ID, got nil")
	}
	if err.Error() != fmt.Sprintf("chunk 99 not found for location ID %d", locationID) {
		t.Errorf("Expected error 'chunk 99 not found for location ID %d', got %q", locationID, err.Error())
	}
}

func TestBinaryCache_LRU_Eviction(t *testing.T) {
	cache, _ := setupTestCache(t, 512, 256) // Max 2 chunks (2 * 256)
	defer closeCacheFiles(t, cache)

	var counter int64
	cache.setTimestampSource(func() int64 {
		counter++
		return counter
	})

	data1 := make([]byte, 256) // Chunk 1
	data2 := make([]byte, 256) // Chunk 2
	data3 := make([]byte, 256) // Chunk 3

	// Write chunk 1 (timestamp 1)
	err := cache.writeChunk(1, 1, data1)
	if err != nil {
		t.Fatalf("Failed to write chunk 1: %v", err)
	}
	if cache.cacheSize != 256 {
		t.Errorf("Expected cache size 256, got %d", cache.cacheSize)
	}
	if cache.lruQueue.Len() != 1 {
		t.Errorf("Expected lruQueue length 1, got %d", cache.lruQueue.Len())
	}

	// Write chunk 2 (timestamp 2)
	err = cache.writeChunk(1, 2, data2)
	if err != nil {
		t.Fatalf("Failed to write chunk 2: %v", err)
	}
	if cache.cacheSize != 512 {
		t.Errorf("Expected cache size 512, got %d", cache.cacheSize)
	}
	if cache.lruQueue.Len() != 2 {
		t.Errorf("Expected lruQueue length 2, got %d", cache.lruQueue.Len())
	}

	// Read chunk 1 to make it most recently used (timestamp 3)
	_, err = cache.readChunk(1, 1)
	if err != nil {
		t.Fatalf("Failed to read chunk 1: %v", err)
	}

	// Write chunk 3 - this should evict the least recently used, which is chunk 2 (timestamp 4)
	err = cache.writeChunk(1, 3, data3)
	if err != nil {
		t.Fatalf("Failed to write chunk 3: %v", err)
	}
	if cache.cacheSize != 512 { // Size should remain max after eviction
		t.Errorf("Expected cache size 512 after eviction, got %d", cache.cacheSize)
	}
	if cache.lruQueue.Len() != 2 { // Should still have 2 items
		t.Errorf("Expected lruQueue length 2 after eviction, got %d", cache.lruQueue.Len())
	}

	// Check that chunk 2 was evicted (should return error)
	_, err = cache.readChunk(1, 2)
	if err == nil {
		t.Error("Expected chunk 2 to be evicted, but it was not found")
	}

	// Check that chunk 1 and chunk 3 are still present
	_, err = cache.readChunk(1, 1)
	if err != nil {
		t.Errorf("Chunk 1 should still be present, but got error: %v", err)
	}
	_, err = cache.readChunk(1, 3)
	if err != nil {
		t.Errorf("Chunk 3 should still be present, but got error: %v", err)
	}

	// The evicted space should have been reused immediately by the new write.
	// So, the evictionList should be empty.
	if len(cache.evictionList) != 0 {
		t.Errorf("Expected 0 items in eviction list, got %d", len(cache.evictionList))
	}
}

func TestBinaryCache_LRU_UpdateOnRead(t *testing.T) {
	cache, _ := setupTestCache(t, 768, 256) // Max 3 chunks
	defer closeCacheFiles(t, cache)

	var counter int64
	cache.setTimestampSource(func() int64 {
		counter++
		return counter
	})

	// Write 3 chunks
	err := cache.writeChunk(1, 1, make([]byte, 256)) // ts 1
	if err != nil {
		t.Fatalf("Failed to write chunk 1: %v", err)
	}
	err = cache.writeChunk(1, 2, make([]byte, 256)) // ts 2
	if err != nil {
		t.Fatalf("Failed to write chunk 2: %v", err)
	}
	err = cache.writeChunk(1, 3, make([]byte, 256)) // ts 3
	if err != nil {
		t.Fatalf("Failed to write chunk 3: %v", err)
	}

	// Read chunk 1, making it MRU (ts 4)
	_, err = cache.readChunk(1, 1)
	if err != nil {
		t.Fatalf("Failed to read chunk 1: %v", err)
	}

	// Read chunk 2, making it MRU (ts 5)
	_, err = cache.readChunk(1, 2)
	if err != nil {
		t.Fatalf("Failed to read chunk 2: %v", err)
	}

	// Write a new chunk, chunk 4. This should evict chunk 3 (the oldest not touched). (ts 6)
	err = cache.writeChunk(1, 4, make([]byte, 256))
	if err != nil {
		t.Fatalf("Failed to write chunk 4: %v", err)
	}

	// Verify chunk 3 is evicted
	_, err = cache.readChunk(1, 3)
	if err == nil {
		t.Error("Expected chunk 3 to be evicted, but it was found")
	}

	// Verify chunk 1, 2, and 4 are still present
	_, err = cache.readChunk(1, 1)
	if err != nil {
		t.Errorf("Chunk 1 should be present: %v", err)
	}
	_, err = cache.readChunk(1, 2)
	if err != nil {
		t.Errorf("Chunk 2 should be present: %v", err)
	}
	_, err = cache.readChunk(1, 4)
	if err != nil {
		t.Errorf("Chunk 4 should be present: %v", err)
	}

	if cache.lruQueue.Len() != 3 {
		t.Errorf("Expected LRU queue length 3, got %d", cache.lruQueue.Len())
	}
	if len(cache.lruMap) != 3 {
		t.Errorf("Expected LRU map length 3, got %d", len(cache.lruMap))
	}
}

func TestBinaryCache_WriteChunk_Overwrite(t *testing.T) {
	cache, _ := setupTestCache(t, 1024, 256) // Max 4 chunks
	defer closeCacheFiles(t, cache)

	var counter int64
	cache.setTimestampSource(func() int64 {
		counter++
		return counter
	})

	locationID := int64(1)
	chunkID := int64(10)
	initialData := []byte("original data") // Length 13, occupies 1 padded chunk (256B)

	// Make updatedData cross fixedChunkSize to properly test part handling and size updates
	updatedData := make([]byte, 300) // Length 300, will occupy 2 padded chunks (512B)
	for i := range updatedData {
		updatedData[i] = byte(i % 256)
	}

	// Initial write (timestamp 1)
	err := cache.writeChunk(locationID, chunkID, initialData)
	if err != nil {
		t.Fatalf("Initial write failed: %v", err)
	}
	if cache.cacheSize != 256 {
		t.Errorf("Expected initial cacheSize to be 256, got %d", cache.cacheSize)
	}
	if cache.lruQueue.Len() != 1 {
		t.Errorf("Expected lruQueue length 1, got %d", cache.lruQueue.Len())
	}
	if len(cache.lruMap) != 1 {
		t.Errorf("Expected lruMap length 1, got %d", len(cache.lruMap))
	}
	if len(cache.evictionList) != 0 {
		t.Errorf("Expected evictionList empty, got %d", len(cache.evictionList))
	}
	oldMeta := cache.metadata[locationID][chunkID]
	if len(oldMeta) != 1 || oldMeta[0].Offset != 0 { // Assuming first chunk starts at 0
		t.Fatalf("Initial metadata not as expected: %+v", oldMeta)
	}

	// Overwrite the same chunk (timestamp 2)
	err = cache.writeChunk(locationID, chunkID, updatedData)
	if err != nil {
		t.Fatalf("Overwrite failed: %v", err)
	}

	// Verify data is updated
	readData, err := cache.readChunk(locationID, chunkID) // This read updates timestamp to 3
	if err != nil {
		t.Fatalf("Failed to read overwritten chunk: %v", err)
	}
	if !bytes.Equal(updatedData, readData) {
		t.Errorf("Overwritten data mismatch: expected %q, got %q", updatedData, readData)
	}

	// Verify cache size and eviction list are correct after overwrite.
	// initialData was 1 part (256B padded). updatedData is 2 parts (2 * 256B padded).
	// The original 256B space (offset 0) is freed and added to evictionList.
	// The first part of updatedData reuses offset 0. The second part gets a new offset.
	// Thus, the evictionList should be empty after the write completes.
	expectedCacheSize := int64(2) * cache.fixedChunkSize
	if cache.cacheSize != expectedCacheSize {
		t.Errorf("Expected cacheSize after overwrite %d, got %d", expectedCacheSize, cache.cacheSize)
	}
	if len(cache.evictionList) != 0 { // Corrected: should be 0 because freed space is reused
		t.Errorf("Expected 0 items in evictionList after overwrite, got %d", len(cache.evictionList))
	}

	// Verify LRU state
	if cache.lruQueue.Len() != 1 { // Still only one logical chunk (chunk 10)
		t.Errorf("Expected lruQueue length 1 after overwrite, got %d", cache.lruQueue.Len())
	}
	if len(cache.lruMap) != 1 {
		t.Errorf("Expected lruMap length 1 after overwrite, got %d", len(cache.lruMap))
	}
	// Check that the single LRU item has the latest timestamp
	lruItem := (*cache.lruQueue)[0]
	if lruItem.locationID != locationID || lruItem.chunkID != chunkID {
		t.Errorf("LRU item mismatch after overwrite: expected %d:%d, got %d:%d", locationID, chunkID, lruItem.locationID, lruItem.chunkID)
	}
}

func TestBinaryCache_MetadataPersistence(t *testing.T) {
	tempDir := t.TempDir()

	// Use a controlled timestamp source for deterministic testing
	var counter int64
	timestampSource := func() int64 {
		counter++
		return counter
	}

	// 1. Initialize, write data, and close
	cache1, err := NewBinaryCache(tempDir, 2048, 256) // Enough space for multiple chunks
	if err != nil {
		t.Fatalf("Failed to initialize BinaryCache (1st time): %v", err)
	}
	cache1.setTimestampSource(timestampSource)

	loc1, chunk1 := int64(100), int64(1)
	data1 := []byte("This is the first chunk of data for location 100.")
	err = cache1.writeChunk(loc1, chunk1, data1) // timestamp 1
	if err != nil {
		t.Fatalf("Failed to write chunk1 (1st time): %v", err)
	}

	loc2, chunk2 := int64(200), int64(5)
	data2 := make([]byte, 300) // Will be split into 2 parts
	for i := range data2 {
		data2[i] = 'a' + byte(i%26)
	}
	err = cache1.writeChunk(loc2, chunk2, data2) // timestamp 2
	if err != nil {
		t.Fatalf("Failed to write chunk2 (1st time): %v", err)
	}

	// Access loc1, chunk1 again to make it most recently used (timestamp 3)
	// This read will also save the metadata, which is key.
	_, err = cache1.readChunk(loc1, chunk1) // timestamp 3 (this read also saves metadata)
	if err != nil {
		t.Fatalf("Failed to read chunk1 (1st time) for LRU update: %v", err)
	}

	// Write another chunk. This will trigger a saveMetadata() *after* timestamp 3 for loc1:chunk1 is saved.
	loc3, chunk3 := int64(300), int64(10)
	data3 := []byte("small third chunk")         // One part
	err = cache1.writeChunk(loc3, chunk3, data3) // timestamp 4
	if err != nil {
		t.Fatalf("Failed to write chunk3 (1st time): %v", err)
	}

	// Before closing, check in-memory state
	expectedCacheSize1 := 1*cache1.fixedChunkSize + 2*cache1.fixedChunkSize + 1*cache1.fixedChunkSize // 4 parts total
	if cache1.cacheSize != expectedCacheSize1 {
		t.Errorf("Cache1: Expected cacheSize %d, got %d", expectedCacheSize1, cache1.cacheSize)
	}
	if cache1.lruQueue.Len() != 3 {
		t.Errorf("Cache1: Expected lruQueue length 3, got %d", cache1.lruQueue.Len())
	}
	if len(cache1.lruMap) != 3 {
		t.Errorf("Cache1: Expected lruMap length 3, got %d", len(cache1.lruMap))
	}
	closeCacheFiles(t, cache1) // Explicitly close

	// 2. Re-open the cache and verify state
	// Do NOT set timestampSource for cache2, and do NOT perform readChunk operations,
	// as that would alter the LRU state before verification.
	cache2, err := NewBinaryCache(tempDir, 2048, 256)
	if err != nil {
		t.Fatalf("Failed to reinitialize BinaryCache (2nd time): %v", err)
	}
	defer closeCacheFiles(t, cache2)

	// Verify loaded cache size
	if cache2.cacheSize != expectedCacheSize1 {
		t.Errorf("Cache2: Expected loaded cacheSize %d, got %d", expectedCacheSize1, cache2.cacheSize)
	}

	// Verify loaded LRU order based on controlled timestamps:
	// T1: loc1:chunk1 (initial write)
	// T2: loc2:chunk2 (written after T1)
	// T3: loc1:chunk1 (READ, timestamp updated) -> this becomes newer than T2
	// T4: loc3:chunk3 (written after T3) -> this becomes the newest
	//
	// So, the final timestamps are:
	// loc1:chunk1 -> T3
	// loc2:chunk2 -> T2
	// loc3:chunk3 -> T4
	//
	// Order (oldest first): loc2:chunk2 (T2), loc1:chunk1 (T3), loc3:chunk3 (T4)
	if cache2.lruQueue.Len() != 3 {
		t.Errorf("Cache2: Expected loaded lruQueue length 3, got %d", cache2.lruQueue.Len())
	}
	if len(cache2.lruMap) != 3 {
		t.Errorf("Cache2: Expected loaded lruMap length 3, got %d", len(cache2.lruMap))
	}

	// Pop elements and check order.
	item1 := heap.Pop(cache2.lruQueue).(*LRUItem)
	if item1.locationID != loc2 || item1.chunkID != chunk2 {
		t.Errorf("Expected oldest item to be %d:%d, got %d:%d", loc2, chunk2, item1.locationID, item1.chunkID)
	}

	item2 := heap.Pop(cache2.lruQueue).(*LRUItem)
	if item2.locationID != loc1 || item2.chunkID != chunk1 {
		t.Errorf("Expected second oldest item to be %d:%d, got %d:%d", loc1, chunk1, item2.locationID, item2.chunkID)
	}

	item3 := heap.Pop(cache2.lruQueue).(*LRUItem)
	if item3.locationID != loc3 || item3.chunkID != chunk3 {
		t.Errorf("Expected newest item to be %d:%d, got %d:%d", loc3, chunk3, item3.locationID, item3.chunkID)
	}
}

func TestBinaryCache_ConcurrentAccess(t *testing.T) {
	cache, _ := setupTestCache(t, 2*1024*1024, 256*1024) // 2MB cache, 256KB fixed chunks
	defer closeCacheFiles(t, cache)

	// For concurrent access, use the real timestamp source, as it's testing concurrency
	// not deterministic LRU order after restart. No need for artificial sleeps here as well.
	cache.setTimestampSource(time.Now().UnixNano)

	numWorkers := 10
	numOperations := 100

	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers*2) // Buffer for errors

	// Concurrent writes
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				locationID := int64(workerID)
				chunkID := int64(j)
				data := bytes.Repeat([]byte{byte(workerID + j%256)}, 100) // Small data, consistent size
				err := cache.writeChunk(locationID, chunkID, data)
				if err != nil {
					errCh <- fmt.Errorf("worker %d: write failed for %d:%d: %v", workerID, locationID, chunkID, err)
					return
				}
			}
		}(i)
	}

	// Concurrent reads (after some writes have likely occurred)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				locationID := int64(workerID)
				chunkID := int64(j)
				// Read a random chunk, not necessarily one just written by this worker
				_, err := cache.readChunk(locationID, chunkID)
				// It's okay if a read returns an error for "not found" if it's not yet written or already evicted.
				// We are primarily testing for race conditions (panics, corrupted data) rather than functional success of every read.
				if err != nil && !bytes.Contains([]byte(err.Error()), []byte("not found")) {
					errCh <- fmt.Errorf("worker %d: read failed for %d:%d: %v", workerID, locationID, chunkID, err)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Concurrent access error: %v", err)
	}

	// Basic check after all operations (no specific integrity check, but ensure no panics)
	if cache.cacheSize == 0 {
		t.Errorf("Cache size should not be zero after concurrent writes")
	}
	if cache.lruQueue.Len() == 0 {
		t.Errorf("LRU queue should not be empty after concurrent writes")
	}
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
