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

	// Fields for asynchronous metadata saving
	saveQueue chan struct{}
	closeChan chan struct{}
	wg        sync.WaitGroup
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
		timestampSource: time.Now().UnixNano,    // Default to real-time clock
		saveQueue:       make(chan struct{}, 1), // Buffered channel of size 1
		closeChan:       make(chan struct{}),
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

	// Start the background goroutine for saving metadata
	bc.wg.Add(1)
	go bc.processSaves()

	return bc, nil
}

// Close gracefully shuts down the BinaryCache, ensuring pending metadata is saved.
func (bc *BinaryCache) Close() error {
	close(bc.closeChan) // Signal the background worker to stop
	bc.wg.Wait()        // Wait for the background worker to finish

	var errs []error
	if err := bc.cashFile.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing cache file: %w", err))
	}
	if err := bc.metadataFile.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing metadata file: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors while closing cache: %v", errs)
	}
	return nil
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

	// Trigger an asynchronous save
	bc.requestSave()
	return nil
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
			continue
		}

		if metas, exists := locationChunks[item.chunkID]; exists {
			for _, meta := range metas {
				bc.evictionList = append(bc.evictionList, meta.Offset)
				bc.cacheSize -= bc.fixedChunkSize
			}
			delete(locationChunks, item.chunkID)

			if len(locationChunks) == 0 {
				delete(bc.metadata, item.locationID)
			}
		}
	}
}

// requestSave sends a non-blocking request to the save queue.
func (bc *BinaryCache) requestSave() {
	select {
	case bc.saveQueue <- struct{}{}:
		// Request sent successfully.
	default:
		// Queue is full, a save is already pending. Do nothing.
	}
}

// processSaves is the background worker that handles debounced metadata saving.
func (bc *BinaryCache) processSaves() {
	defer bc.wg.Done()
	const debounceDuration = 2 * time.Second
	var timer *time.Timer

	for {
		select {
		case <-bc.saveQueue:
			// A save has been requested. Reset the timer to wait for more potential updates.
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(debounceDuration)
		case <-bc.closeChan:
			// The cache is closing.
			if timer != nil {
				timer.Stop()
			}
			fmt.Println("Shutting down cache... performing final metadata save.")
			bc.saveMetadataInternal()
			return
		case <-func() <-chan time.Time {
			if timer == nil {
				return nil // Block forever if timer is not set
			}
			return timer.C
		}():
			// The timer fired, so it's time to save.
			fmt.Println("Debounce timer fired, saving metadata...")
			bc.saveMetadataInternal()
			timer = nil // Mark timer as inactive
		}
	}
}

// saveMetadataInternal performs the actual synchronous saving of metadata to disk.
func (bc *BinaryCache) saveMetadataInternal() {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	_, err := bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		fmt.Printf("Error seeking metadata file: %v\n", err)
		return
	}

	err = bc.metadataFile.Truncate(0)
	if err != nil {
		fmt.Printf("Error truncating metadata file: %v\n", err)
		return
	}

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

	sort.Slice(allLogicalChunks, func(i, j int) bool {
		if allLogicalChunks[i].LocationID != allLogicalChunks[j].LocationID {
			return allLogicalChunks[i].LocationID < allLogicalChunks[j].LocationID
		}
		return allLogicalChunks[i].ChunkID < allLogicalChunks[j].ChunkID
	})

	totalLogicalChunks := int64(len(allLogicalChunks))
	if err := binary.Write(bc.metadataFile, binary.LittleEndian, totalLogicalChunks); err != nil {
		fmt.Printf("Error writing total chunk count to metadata: %v\n", err)
		return
	}

	for _, lChunk := range allLogicalChunks {
		if err := binary.Write(bc.metadataFile, binary.LittleEndian, lChunk.LocationID); err != nil {
			fmt.Printf("Error writing LocationID to metadata: %v\n", err)
			return
		}
		if err := binary.Write(bc.metadataFile, binary.LittleEndian, lChunk.ChunkID); err != nil {
			fmt.Printf("Error writing ChunkID to metadata: %v\n", err)
			return
		}
		if err := binary.Write(bc.metadataFile, binary.LittleEndian, int64(len(lChunk.Metas))); err != nil {
			fmt.Printf("Error writing part count to metadata: %v\n", err)
			return
		}

		for _, meta := range lChunk.Metas {
			if err := binary.Write(bc.metadataFile, binary.LittleEndian, &meta); err != nil {
				fmt.Printf("Error writing chunk part metadata: %v\n", err)
				return
			}
		}
	}

	if err := bc.metadataFile.Sync(); err != nil {
		fmt.Printf("Error syncing metadata file: %v\n", err)
	}
}

// loadMetadata loads metadata from the metadata file.
func (bc *BinaryCache) loadMetadata() error {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	fileInfo, err := bc.metadataFile.Stat()
	if err != nil {
		return err
	}

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
		return bc.initializeFile()
	}

	bc.metadata = make(map[int64]map[int64][]chunkMetadata)
	bc.cacheSize = 0
	bc.lruQueue = &PriorityQueue{}
	bc.lruMap = make(map[string]*LRUItem)

	for i := int64(0); i < totalLogicalChunks; i++ {
		var locationID, chunkID, partCount int64

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
			if err := binary.Read(bc.metadataFile, binary.LittleEndian, &meta); err != nil {
				return fmt.Errorf("failed to read part metadata for part %d of logical chunk %d: %w", p, i, err)
			}

			if p == 0 {
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

	heap.Init(bc.lruQueue)
	return nil
}

// initializeFile clears and sets up an empty metadata file.
func (bc *BinaryCache) initializeFile() error {
	if err := bc.metadataFile.Truncate(0); err != nil {
		return err
	}
	if _, err := bc.metadataFile.Seek(0, os.SEEK_SET); err != nil {
		return err
	}
	var numLogicalChunks int64 = 0
	if err := binary.Write(bc.metadataFile, binary.LittleEndian, numLogicalChunks); err != nil {
		return err
	}
	bc.metadata = make(map[int64]map[int64][]chunkMetadata)
	bc.cacheSize = 0
	bc.lruQueue = &PriorityQueue{}
	bc.lruMap = make(map[string]*LRUItem)
	heap.Init(bc.lruQueue)
	return bc.metadataFile.Sync()
}
