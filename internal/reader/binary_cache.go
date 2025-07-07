package reader

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
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
	meta.Timestamp = t.Unix()
}

func (meta *chunkMetadata) GetTimestamp() time.Time {
	return time.Unix(meta.Timestamp, 0)
}

type BinaryCache struct {
	cashFile       *os.File
	metadataFile   *os.File
	metadata       map[int64]map[int64][]chunkMetadata // Map of location ID to chunk ID to a slice of its parts' metadata
	metadataLock   sync.Mutex
	chunkLock      sync.Mutex
	cacheSize      int64
	maxCacheSize   int64
	lruQueue       *PriorityQueue
	evictionList   []*chunkMetadata // List of offsets available for reuse
	fixedChunkSize int64
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
	return pq[i].timestamp < pq[j].timestamp
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
	item.index = -1 // For safety
	*pq = old[0 : n-1]
	return item
}

func (pq *PriorityQueue) update(item *LRUItem, timestamp int64) {
	item.timestamp = timestamp
	heap.Fix(pq, item.index)
}

// NewBinaryCache initializes a new binary cache
func NewBinaryCache(cacheDir string, maxCacheSize int64, fixedChunkSize int64) (*BinaryCache, error) {
	// Create the cache directory if it doesn't exist
	err := os.MkdirAll(cacheDir, 0755)
	if err != nil {
		return nil, err
	}

	// Define the file paths for cache and metadata
	cacheFilename := filepath.Join(cacheDir, "cache.dat")
	metadataFilename := filepath.Join(cacheDir, "metadata.dat")

	// Open or create the cache file
	file, err := os.OpenFile(cacheFilename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	// Open or create the metadata file
	metadataFile, err := os.OpenFile(metadataFilename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		file.Close()
		return nil, err
	}

	// Initialize the BinaryCache struct
	bc := &BinaryCache{
		cashFile:       file,
		metadataFile:   metadataFile,
		metadata:       make(map[int64]map[int64][]chunkMetadata),
		maxCacheSize:   maxCacheSize,
		lruQueue:       &PriorityQueue{},
		fixedChunkSize: fixedChunkSize,
	}

	// Load metadata from the metadata file if it exists
	err = bc.loadMetadata()
	if err != nil {
		return nil, err
	}

	// Initialize the priority queue (LRU queue) from loaded metadata
	// heap.Init(bc.lruQueue) // This is implicitly done by addLRU in loadMetadata, but calling it here ensures heap properties are correct if items are added manually too.

	return bc, nil
}

// Write a logical chunk (which may consist of multiple fixed-size parts) to the binary cashFile.
func (bc *BinaryCache) writeChunk(locationID int64, chunkID int64, chunk []byte) error {
	bc.chunkLock.Lock()
	defer bc.chunkLock.Unlock()

	// Ensure the locationID map exists
	if _, exists := bc.metadata[locationID]; !exists {
		bc.metadata[locationID] = make(map[int64][]chunkMetadata)
	}

	// If this chunk already exists, remove its old metadata and free space for reuse.
	if oldMetas, exists := bc.metadata[locationID][chunkID]; exists {
		for _, meta := range oldMetas {
			bc.evictionList = append(bc.evictionList, &meta) // Add to the list of evicted offsets
			bc.cacheSize -= bc.fixedChunkSize
		}
		delete(bc.metadata[locationID], chunkID) // Remove old logical chunk entry
	}

	// Evict if cache size exceeds max size BEFORE writing new data
	bc.evictIfNeeded()

	// Split the incoming chunk into fixed-sized parts
	chunkParts := bc.splitChunk(chunk)

	newChunkMetas := make([]chunkMetadata, 0, len(chunkParts))
	timestamp := time.Now().Unix() // Use a single timestamp for the whole logical chunk

	// Write each part to the cashFile and collect its metadata
	for i, part := range chunkParts {
		offset, err := bc.getWriteOffset() // Get an offset for this part (reuse or append)
		if err != nil {
			return err
		}

		paddedPart := make([]byte, bc.fixedChunkSize)
		copy(paddedPart, part) // Pad the part to the fixed chunk size

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
	bc.metadata[locationID][chunkID] = newChunkMetas
	bc.addLRU(locationID, chunkID, timestamp) // Add once for the entire logical chunk

	// Save the metadata to the metadata file
	return bc.saveMetadata()
}

// Helper to get an offset for writing a chunk part (reuse an evicted slot or append to end)
func (bc *BinaryCache) getWriteOffset() (int64, error) {
	if len(bc.evictionList) > 0 {
		evictedMeta := bc.evictionList[len(bc.evictionList)-1]
		bc.evictionList = bc.evictionList[:len(bc.evictionList)-1] // Remove the last element
		return evictedMeta.Offset, nil
	}
	return bc.cashFile.Seek(0, os.SEEK_END) // Append to end of file
}

// Helper method to split the chunk into fixed-size parts
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

// Read a specific logical chunk from the binary cashFile
func (bc *BinaryCache) readChunk(locationID int64, chunkID int64) ([]byte, error) {
	bc.chunkLock.Lock()
	defer bc.chunkLock.Unlock()

	locationMetadata, exists := bc.metadata[locationID]
	if !exists {
		return nil, fmt.Errorf("location ID %d not found", locationID)
	}

	// Retrieve all parts' metadata for the logical chunk
	chunkMetas, exists := locationMetadata[chunkID]
	if !exists {
		return nil, fmt.Errorf("chunk %d not found for location ID %d", chunkID, locationID)
	}

	// Combine all parts in order
	var chunk []byte
	for _, meta := range chunkMetas {
		part, err := bc.readChunkPart(meta)
		if err != nil {
			return nil, err
		}
		chunk = append(chunk, part...)
	}

	// Update the timestamp for LRU for this logical chunk
	timestamp := time.Now().Unix()
	bc.updateLRU(locationID, chunkID, timestamp)

	return chunk, nil
}

// Helper method to read a single part of a chunk
func (bc *BinaryCache) readChunkPart(meta chunkMetadata) ([]byte, error) {
	// Seek to the part's offset
	_, err := bc.cashFile.Seek(meta.Offset, os.SEEK_SET)
	if err != nil {
		return nil, err
	}

	// Read the part's data (padded size)
	paddedPart := make([]byte, bc.fixedChunkSize)
	_, err = bc.cashFile.Read(paddedPart)
	if err != nil {
		return nil, err
	}

	// Return only the actual size of the data, trimming any padding
	return paddedPart[:meta.Size], nil
}

// Add a logical chunk to the LRU queue
func (bc *BinaryCache) addLRU(locationID int64, chunkID int64, timestamp int64) {
	item := &LRUItem{
		locationID: locationID,
		chunkID:    chunkID,
		timestamp:  timestamp,
	}
	heap.Push(bc.lruQueue, item)
}

// Update a logical chunk's position in the LRU queue
func (bc *BinaryCache) updateLRU(locationID int64, chunkID int64, timestamp int64) {
	// Find the item in the heap and update its timestamp, then fix the heap.
	// This approach might be slow if the LRU queue is very large.
	for _, item := range *bc.lruQueue {
		if item.locationID == locationID && item.chunkID == chunkID {
			bc.lruQueue.update(item, timestamp)
			return
		}
	}
	// If item not found (e.g., if it was evicted and then re-read), add it again
	bc.addLRU(locationID, chunkID, timestamp)
}

// Evict logical chunks until the cache size is within the limit
func (bc *BinaryCache) evictIfNeeded() {
	for bc.cacheSize >= bc.maxCacheSize && bc.lruQueue.Len() > 0 {
		// Evict the least recently used logical chunk
		item := heap.Pop(bc.lruQueue).(*LRUItem)

		// Get all metadata parts for this logical chunk
		if metas, exists := bc.metadata[item.locationID][item.chunkID]; exists {
			for _, meta := range metas {
				bc.evictionList = append(bc.evictionList, &meta) // Add each part's metadata to the eviction list
				bc.cacheSize -= bc.fixedChunkSize                // Decrement cache size by the fixed size of each part
			}
			delete(bc.metadata[item.locationID], item.chunkID) // Remove the logical chunk entry from metadata map
			if len(bc.metadata[item.locationID]) == 0 {
				delete(bc.metadata, item.locationID) // Remove location entry if no chunks left
			}
		}
	}
}

// Save metadata to the metadata file
// This format now stores logical chunks, each with their part count and then parts' metadata.
func (bc *BinaryCache) saveMetadata() error {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	_, err := bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	// Clear the metadata file before saving new data
	err = bc.metadataFile.Truncate(0)
	if err != nil {
		return err
	}

	// Count total logical chunks
	totalLogicalChunks := int64(0)
	for _, locationChunks := range bc.metadata {
		totalLogicalChunks += int64(len(locationChunks))
	}

	err = binary.Write(bc.metadataFile, binary.LittleEndian, totalLogicalChunks)
	if err != nil {
		return err
	}

	for locationID, locationChunks := range bc.metadata {
		for chunkID, metas := range locationChunks {
			// Write logical chunk header: locationID, chunkID, number of parts
			err := binary.Write(bc.metadataFile, binary.LittleEndian, locationID)
			if err != nil {
				return err
			}
			err = binary.Write(bc.metadataFile, binary.LittleEndian, chunkID)
			if err != nil {
				return err
			}
			err = binary.Write(bc.metadataFile, binary.LittleEndian, int64(len(metas))) // Number of parts for this logical chunk
			if err != nil {
				return err
			}

			// Write each part's metadata
			for _, meta := range metas {
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
	}

	return bc.metadataFile.Sync()
}

// Load metadata from the metadata file
func (bc *BinaryCache) loadMetadata() error {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	fileInfo, err := bc.metadataFile.Stat()
	if err != nil {
		return err
	}
	fileSize := fileInfo.Size()

	// If file is empty or likely corrupted (e.g., cannot read initial chunk count), reinitialize.
	if fileSize == 0 {
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

	// Reset in-memory metadata before loading
	bc.metadata = make(map[int64]map[int64][]chunkMetadata)
	bc.cacheSize = 0

	for i := int64(0); i < totalLogicalChunks; i++ {
		var locationID int64
		var chunkID int64
		var partCount int64

		// Read logical chunk header
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
		var chunkTimestamp int64 // Will store the timestamp for the LRU item for this logical chunk

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

			if p == 0 { // Use timestamp of the first part for the logical chunk's LRU
				chunkTimestamp = meta.Timestamp
			}
			metasForLogicalChunk[p] = meta
			bc.cacheSize += bc.fixedChunkSize
		}

		if _, exists := bc.metadata[locationID]; !exists {
			bc.metadata[locationID] = make(map[int64][]chunkMetadata)
		}
		bc.metadata[locationID][chunkID] = metasForLogicalChunk
		bc.addLRU(locationID, chunkID, chunkTimestamp) // Add LRU item for the logical chunk
	}

	return nil
}

// Initialize the metadata file
func (bc *BinaryCache) initializeFile() error {
	// Truncate the metadata file to clear existing data
	err := bc.metadataFile.Truncate(0)
	if err != nil {
		return err
	}

	_, err = bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	// Initialize with zero logical chunks
	var numLogicalChunks int64 = 0
	err = binary.Write(bc.metadataFile, binary.LittleEndian, numLogicalChunks)
	if err != nil {
		return err
	}

	// Reset in-memory metadata
	bc.metadata = make(map[int64]map[int64][]chunkMetadata)
	bc.cacheSize = 0
	bc.lruQueue = &PriorityQueue{} // Reset LRU queue too
	heap.Init(bc.lruQueue)         // Initialize the new empty heap

	// Ensure changes are written to disk
	err = bc.metadataFile.Sync()
	if err != nil {
		return err
	}

	return nil
}
