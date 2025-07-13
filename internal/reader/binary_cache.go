package reader

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type chunkMetadata struct {
	LocationID int64
	ChunkIndex int64 // Index of this part within the logical chunk
	Offset     int64
	Size       int64 // Actual size of the data in this part, not the padded size
	Timestamp  int64 // Timestamp for LRU
}

// Helper methods for converting the `Timestamp` to/from `time.Time`
func (meta *chunkMetadata) SetTimestamp(t time.Time) {
	meta.Timestamp = t.UnixNano()
}

func (meta *chunkMetadata) GetTimestamp() time.Time {
	return time.Unix(0, meta.Timestamp)
}

type BinaryCache struct {
	cashFile        *os.File
	metadataFile    *os.File
	metadata        map[int64]map[int64][]chunkMetadata // Map of location ID to chunk ID to a slice of its parts' metadata
	metadataLock    sync.Mutex
	chunkLock       sync.Mutex // Protects cacheSize, evictionList, and overall write/read operations
	cacheSize       int64
	maxCacheSize    int64
	lruQueue        *PriorityQueue
	lruMap          map[string]*LRUItem // Map for O(1) LRU item lookup
	evictionList    []int64             // List of OFFSETS available for reuse
	fixedChunkSize  int64
	timestampSource func() int64 // Function to get current timestamp for testability
}

// LRUItem represents an item in the LRU cache with its priority.
type LRUItem struct {
	locationID int64
	chunkID    int64
	timestamp  int64
	index      int // The index of the item in the heap.
}

// PriorityQueue implements a min-heap for LRU eviction.
type PriorityQueue []*LRUItem

func (pq PriorityQueue) Len() int { return len(pq) }

func (pq PriorityQueue) Less(i, j int) bool {
	// Sort by timestamp first (min-heap: smallest timestamp is "less")
	if pq[i].timestamp != pq[j].timestamp {
		return pq[i].timestamp < pq[j].timestamp
	}
	// If timestamps are identical, use locationID and then chunkID as tie-breakers
	// to ensure deterministic ordering in tests.
	if pq[i].locationID != pq[j].locationID {
		return pq[i].locationID < pq[j].locationID
	}
	return pq[i].chunkID < pq[j].chunkID
}

func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *PriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*LRUItem)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // Avoid memory leak
	item.index = -1 // For safety, mark as no longer in heap
	*pq = old[0 : n-1]
	return item
}

// This function is no longer called directly for updating item priority.
// Instead, the item is effectively "re-added" via updateLRU.
// It is kept for reference but is not currently used.
func (pq *PriorityQueue) update(item *LRUItem, timestamp int64) {
	item.timestamp = timestamp
	heap.Fix(pq, item.index)
}

// Helper to generate a unique key for LRU map
func lruKey(locationID, chunkID int64) string {
	return fmt.Sprintf("%d:%d", locationID, chunkID)
}

func NewBinaryCache(cacheDir string, maxCacheSize int64, fixedChunkSize int64) (*BinaryCache, error) {
	err := os.MkdirAll(cacheDir, 0755)
	if err != nil {
		return nil, err
	}

	cacheFilename := filepath.Join(cacheDir, "cache.dat")
	metadataFilename := filepath.Join(cacheDir, "metadata.dat")

	file, err := os.OpenFile(cacheFilename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	metadataFile, err := os.OpenFile(metadataFilename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		file.Close()
		return nil, err
	}

	bc := &BinaryCache{
		cashFile:        file,
		metadataFile:    metadataFile,
		metadata:        make(map[int64]map[int64][]chunkMetadata),
		maxCacheSize:    maxCacheSize,
		lruQueue:        &PriorityQueue{},
		lruMap:          make(map[string]*LRUItem),
		evictionList:    []int64{},
		fixedChunkSize:  fixedChunkSize,
		timestampSource: time.Now().UnixNano, // Default to real-time clock
	}

	// Load metadata from the metadata file if it exists
	err = bc.loadMetadata()
	if err != nil {
		// If loading metadata fails, close files and return error.
		// This implies the cache is unrecoverable or file system issue.
		closeErr := file.Close()
		closeMetaErr := metadataFile.Close()
		if closeErr != nil || closeMetaErr != nil {
			return nil, fmt.Errorf("failed to load metadata (%w), and error closing files: %v, %v", err, closeErr, closeMetaErr)
		}
		return nil, err
	}

	return bc, nil
}

// setTimestampSource is a helper for testing to inject a mock timestamp generator.
func (bc *BinaryCache) setTimestampSource(f func() int64) {
	bc.timestampSource = f
}

// writeChunk writes a logical chunk, which may consist of multiple fixed-size parts,
// to the binary cache file.
func (bc *BinaryCache) writeChunk(locationID int64, chunkID int64, chunk []byte) error {
	bc.chunkLock.Lock()
	defer bc.chunkLock.Unlock()

	// Ensure the locationID map exists for atomicity under lock.
	locationChunks, exists := bc.metadata[locationID]
	if !exists {
		locationChunks = make(map[int64][]chunkMetadata)
		bc.metadata[locationID] = locationChunks
	}

	// If this chunk already exists, remove its old metadata and free space for reuse.
	if oldMetas, exists := locationChunks[chunkID]; exists {
		for _, meta := range oldMetas {
			bc.evictionList = append(bc.evictionList, meta.Offset)
			bc.cacheSize -= bc.fixedChunkSize
		}
		delete(locationChunks, chunkID)

		// Explicitly remove the old LRUItem from the heap when overwriting.
		if oldLruItem, lruMapExists := bc.lruMap[lruKey(locationID, chunkID)]; lruMapExists {
			if oldLruItem.index != -1 { // Only remove if it's currently in heap
				heap.Remove(bc.lruQueue, oldLruItem.index)
			}
			delete(bc.lruMap, lruKey(locationID, chunkID))
		}
	}

	// Evict if cache size exceeds max size BEFORE writing new data
	bc.evictIfNeeded()

	chunkParts := bc.splitChunk(chunk)
	newChunkMetas := make([]chunkMetadata, 0, len(chunkParts))
	timestamp := bc.timestampSource()

	// Write each part to the cache file and collect its metadata
	for i, part := range chunkParts {
		offset, err := bc.getWriteOffset()
		if err != nil {
			return err
		}

		paddedPart := make([]byte, bc.fixedChunkSize)
		copy(paddedPart, part)

		_, err = bc.cashFile.WriteAt(paddedPart, offset)
		if err != nil {
			return err
		}

		meta := chunkMetadata{
			LocationID: locationID,
			ChunkIndex: int64(i), // Index of this part within the logical chunk
			Offset:     offset,
			Size:       int64(len(part)),
			Timestamp:  timestamp, // All parts of a logical chunk share the same timestamp for LRU
		}
		newChunkMetas = append(newChunkMetas, meta)
		bc.cacheSize += bc.fixedChunkSize // Increment cache size by the fixed size of the stored part
	}

	// After all parts are written, add the logical chunk to metadata and LRU queue
	locationChunks[chunkID] = newChunkMetas
	bc.addLRU(locationID, chunkID, timestamp)

	return bc.saveMetadata()
}

// getWriteOffset gets an offset for writing a chunk part, reusing an evicted slot or appending to the end.
func (bc *BinaryCache) getWriteOffset() (int64, error) {
	if len(bc.evictionList) > 0 {
		offset := bc.evictionList[len(bc.evictionList)-1]
		bc.evictionList = bc.evictionList[:len(bc.evictionList)-1]
		return offset, nil
	}
	return bc.cashFile.Seek(0, os.SEEK_END)
}

// splitChunk splits the chunk into fixed-size parts.
func (bc *BinaryCache) splitChunk(chunk []byte) [][]byte {
	var parts [][]byte
	for len(chunk) > 0 {
		partSize := bc.fixedChunkSize
		if int64(len(chunk)) < bc.fixedChunkSize {
			partSize = int64(len(chunk))
		}
		parts = append(parts, chunk[:partSize])
		chunk = chunk[partSize:]
	}
	return parts
}

// readChunk reads a specific logical chunk from the binary cache file.
func (bc *BinaryCache) readChunk(locationID int64, chunkID int64) ([]byte, error) {
	bc.chunkLock.Lock()
	defer bc.chunkLock.Unlock()

	locationMetadata, exists := bc.metadata[locationID]
	if !exists {
		return nil, fmt.Errorf("location ID %d not found", locationID)
	}

	chunkMetas, exists := locationMetadata[chunkID]
	if !exists {
		return nil, fmt.Errorf("chunk %d not found for location ID %d", chunkID, locationID)
	}

	var chunk []byte
	for _, meta := range chunkMetas {
		part, err := bc.readChunkPart(meta)
		if err != nil {
			return nil, err
		}
		chunk = append(chunk, part...)
	}

	timestamp := bc.timestampSource()
	bc.updateLRU(locationID, chunkID, timestamp)

	// Propagate the new timestamp to the actual metadata stored in bc.metadata.
	// This ensures the LRU state is persisted correctly if a writeChunk operation later saves it.
	for i := range bc.metadata[locationID][chunkID] {
		bc.metadata[locationID][chunkID][i].Timestamp = timestamp
	}


	return chunk, nil
}

// readChunkPart reads a single part of a chunk.
func (bc *BinaryCache) readChunkPart(meta chunkMetadata) ([]byte, error) {
	_, err := bc.cashFile.Seek(meta.Offset, os.SEEK_SET)
	if err != nil {
		return nil, err
	}

	// Read the part's data (which is padded to a fixed size).
	paddedPart := make([]byte, bc.fixedChunkSize)
	_, err = bc.cashFile.Read(paddedPart)
	if err != nil {
		return nil, err
	}

	// Return only the actual size of the data, trimming any padding.
	return paddedPart[:meta.Size], nil
}

// addLRU adds a logical chunk to the LRU queue and map.
func (bc *BinaryCache) addLRU(locationID int64, chunkID int64, timestamp int64) {
	item := &LRUItem{
		locationID: locationID,
		chunkID:    chunkID,
		timestamp:  timestamp,
	}
	heap.Push(bc.lruQueue, item)
	bc.lruMap[lruKey(locationID, chunkID)] = item
}

// updateLRU updates a logical chunk's position in the LRU queue by removing and re-adding it.
// This is more robust than `heap.Fix` if an item's index becomes inconsistent.
func (bc *BinaryCache) updateLRU(locationID int64, chunkID int64, timestamp int64) {
	key := lruKey(locationID, chunkID)
	if oldItem, ok := bc.lruMap[key]; ok {
		if oldItem.index != -1 {
			heap.Remove(bc.lruQueue, oldItem.index)
		}
		delete(bc.lruMap, key)
	}
	// Add the item with the new timestamp.
	bc.addLRU(locationID, chunkID, timestamp)
}

// evictIfNeeded evicts logical chunks until the cache size is within the limit.
func (bc *BinaryCache) evictIfNeeded() {
	for bc.cacheSize >= bc.maxCacheSize && bc.lruQueue.Len() > 0 {
		item := heap.Pop(bc.lruQueue).(*LRUItem)
		key := lruKey(item.locationID, item.chunkID)
		delete(bc.lruMap, key) // Delete from map immediately after popping

		locationChunks, locExists := bc.metadata[item.locationID]
		if !locExists {
			// This can happen if a location was removed by a prior operation,
			// leaving a stale entry in the LRU queue.
			continue
		}

		if metas, exists := locationChunks[item.chunkID]; exists {
			for _, meta := range metas {
				bc.evictionList = append(bc.evictionList, meta.Offset)
				bc.cacheSize -= bc.fixedChunkSize
			}
			delete(locationChunks, item.chunkID)

			// If the location map becomes empty after deleting the last chunk, remove it too.
			if len(locationChunks) == 0 {
				delete(bc.metadata, item.locationID)
			}
		} else {
			// This indicates an inconsistency: item popped from LRU but not in metadata.
			// This scenario should ideally not happen if add/remove logic is symmetric.
		}
	}
}

// saveMetadata saves metadata to the metadata file.
// This format stores logical chunks, each with their part count and then parts' metadata.
func (bc *BinaryCache) saveMetadata() error {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	_, err := bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	// Clear the metadata file before saving new data.
	err = bc.metadataFile.Truncate(0)
	if err != nil {
		return err
	}

	// Collect all logical chunk metadata into a slice for deterministic sorting.
	type LogicalChunkData struct {
		LocationID int64
		ChunkID    int64
		Metas      []chunkMetadata
	}
	var allLogicalChunks []LogicalChunkData

	for locationID, locationChunks := range bc.metadata {
		for chunkID, metas := range locationChunks {
			allLogicalChunks = append(allLogicalChunks, LogicalChunkData{
				LocationID: locationID,
				ChunkID:    chunkID,
				Metas:      metas,
			})
		}
	}

	// Sort logical chunks for deterministic writing, which is crucial for testing.
	sort.Slice(allLogicalChunks, func(i, j int) bool {
		if allLogicalChunks[i].LocationID != allLogicalChunks[j].LocationID {
			return allLogicalChunks[i].LocationID < allLogicalChunks[j].LocationID
		}
		return allLogicalChunks[i].ChunkID < allLogicalChunks[j].ChunkID
	})

	totalLogicalChunks := int64(len(allLogicalChunks))
	err = binary.Write(bc.metadataFile, binary.LittleEndian, totalLogicalChunks)
	if err != nil {
		return err
	}

	for _, lChunk := range allLogicalChunks {
		err := binary.Write(bc.metadataFile, binary.LittleEndian, lChunk.LocationID)
		if err != nil {
			return err
		}
		err = binary.Write(bc.metadataFile, binary.LittleEndian, lChunk.ChunkID)
		if err != nil {
			return err
		}
		// Number of parts for this logical chunk
		err = binary.Write(bc.metadataFile, binary.LittleEndian, int64(len(lChunk.Metas)))
		if err != nil {
			return err
		}

		for _, meta := range lChunk.Metas {
			err := binary.Write(bc.metadataFile, binary.LittleEndian, meta.LocationID)
			if err != nil {
				return err
			}
			err = binary.Write(bc.metadataFile, binary.LittleEndian, meta.ChunkIndex)
			if err != nil {
				return err
			}
			err = binary.Write(bc.metadataFile, binary.LittleEndian, meta.Offset)
			if err != nil {
				return err
			}
			err = binary.Write(bc.metadataFile, binary.LittleEndian, meta.Size)
			if err != nil {
				return err
			}
			err = binary.Write(bc.metadataFile, binary.LittleEndian, meta.Timestamp)
			if err != nil {
				return err
			}
		}
	}

	return bc.metadataFile.Sync()
}

// loadMetadata loads metadata from the metadata file.
func (bc *BinaryCache) loadMetadata() error {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	fileInfo, err := bc.metadataFile.Stat()
	if err != nil {
		return err
	}

	// If file is empty or likely corrupted, reinitialize.
	if fileInfo.Size() == 0 {
		return bc.initializeFile()
	}

	_, err = bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	var totalLogicalChunks int64
	err = binary.Read(bc.metadataFile, binary.LittleEndian, &totalLogicalChunks)
	if err != nil {
		// If reading the initial count fails, assume corrupted and reinitialize.
		return bc.initializeFile()
	}

	// Reset in-memory metadata and LRU structures before loading.
	bc.metadata = make(map[int64]map[int64][]chunkMetadata)
	bc.cacheSize = 0
	bc.lruQueue = &PriorityQueue{}
	bc.lruMap = make(map[string]*LRUItem)

	for i := int64(0); i < totalLogicalChunks; i++ {
		var locationID int64
		var chunkID int64
		var partCount int64

		if err := binary.Read(bc.metadataFile, binary.LittleEndian, &locationID); err != nil {
			return fmt.Errorf("failed to read locationID for logical chunk %d: %w", i, err)
		}
		if err := binary.Read(bc.metadataFile, binary.LittleEndian, &chunkID); err != nil {
			return fmt.Errorf("failed to read chunkID for logical chunk %d: %w", i, err)
		}
		if err := binary.Read(bc.metadataFile, binary.LittleEndian, &partCount); err != nil {
			return fmt.Errorf("failed to read partCount for logical chunk %d (loc %d, chunk %d): %w", i, locationID, chunkID, err)
		}

		metasForLogicalChunk := make([]chunkMetadata, partCount)
		var chunkTimestamp int64

		for p := int64(0); p < partCount; p++ {
			var meta chunkMetadata
			if err := binary.Read(bc.metadataFile, binary.LittleEndian, &meta.LocationID); err != nil {
				return fmt.Errorf("failed to read part metadata (locID) for part %d of logical chunk %d: %w", p, i, err)
			}
			if err := binary.Read(bc.metadataFile, binary.LittleEndian, &meta.ChunkIndex); err != nil {
				return fmt.Errorf("failed to read part metadata (chunkIndex) for part %d of logical chunk %d: %w", p, i, err)
			}
			if err := binary.Read(bc.metadataFile, binary.LittleEndian, &meta.Offset); err != nil {
				return fmt.Errorf("failed to read part metadata (offset) for part %d of logical chunk %d: %w", p, i, err)
			}
			if err := binary.Read(bc.metadataFile, binary.LittleEndian, &meta.Size); err != nil {
				return fmt.Errorf("failed to read part metadata (size) for part %d of logical chunk %d: %w", p, i, err)
			}
			if err := binary.Read(bc.metadataFile, binary.LittleEndian, &meta.Timestamp); err != nil {
				return fmt.Errorf("failed to read part metadata (timestamp) for part %d of logical chunk %d: %w", p, i, err)
			}

			if p == 0 { // Use timestamp of the first part for the logical chunk's LRU.
				chunkTimestamp = meta.Timestamp
			}
			metasForLogicalChunk[p] = meta
			bc.cacheSize += bc.fixedChunkSize
		}

		if _, exists := bc.metadata[locationID]; !exists {
			bc.metadata[locationID] = make(map[int64][]chunkMetadata)
		}
		bc.metadata[locationID][chunkID] = metasForLogicalChunk
		bc.addLRU(locationID, chunkID, chunkTimestamp)
	}

	// CRITICAL FIX: Initialize the heap after loading all elements to ensure heap property.
	heap.Init(bc.lruQueue)

	return nil
}

// initializeFile clears and sets up an empty metadata file.
func (bc *BinaryCache) initializeFile() error {
	err := bc.metadataFile.Truncate(0)
	if err != nil {
		return err
	}

	_, err = bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	var numLogicalChunks int64 = 0
	err = binary.Write(bc.metadataFile, binary.LittleEndian, numLogicalChunks)
	if err != nil {
		return err
	}

	// Reset in-memory metadata and LRU structures.
	bc.metadata = make(map[int64]map[int64][]chunkMetadata)
	bc.cacheSize = 0
	bc.lruQueue = &PriorityQueue{}
	bc.lruMap = make(map[string]*LRUItem)
	heap.Init(bc.lruQueue)

	return bc.metadataFile.Sync()
}
