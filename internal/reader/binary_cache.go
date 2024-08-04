package reader

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type chunkMetadata struct {
	LocationID int64
	ChunkIndex int64
	Offset     int64
	Size       int64 // Actual size of the data in this chunk, not the padded size
	Timestamp  int64
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
	metadata       map[int64]map[int64][]chunkMetadata // Map of location ID to chunk ID to metadata
	metadataLock   sync.Mutex
	chunkLock      sync.Mutex
	cacheSize      int64
	maxCacheSize   int64
	lruQueue       *PriorityQueue
	evictionList   []*chunkMetadata
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

	// Initialize the priority queue (LRU queue)
	heap.Init(bc.lruQueue)

	return bc, nil
}

// Write a chunk to the binary cashFile
func (bc *BinaryCache) writeChunk(locationID int64, chunkID int64, chunk []byte) error {
	bc.chunkLock.Lock()
	defer bc.chunkLock.Unlock()

	if _, exists := bc.metadata[locationID]; !exists {
		bc.metadata[locationID] = make(map[int64][]chunkMetadata)
	}

	// Evict if cache size exceeds max size before writing new data
	bc.evictIfNeeded()

	// Split the chunk into fixed-sized chunks
	chunkParts := bc.splitChunk(chunk)

	// Write each part
	for i, part := range chunkParts {
		err := bc.writeChunkPart(locationID, chunkID, int64(i), part)
		if err != nil {
			return err
		}
	}

	// Save the metadata to the metadata file
	return bc.saveMetadata()
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

// Helper method to write a part of the chunk
func (bc *BinaryCache) writeChunkPart(locationID, chunkID, partIndex int64, part []byte) error {
	var offset int64
	var err error

	// Check if we can overwrite an evicted chunk
	if len(bc.evictionList) > 0 {
		evictedMeta := bc.evictionList[len(bc.evictionList)-1]
		bc.evictionList = bc.evictionList[:len(bc.evictionList)-1] // Remove the last element
		offset = evictedMeta.Offset
	} else {
		offset, err = bc.cashFile.Seek(0, os.SEEK_END)
		if err != nil {
			return err
		}
	}

	// Pad the part to the fixed chunk size if necessary
	paddedPart := make([]byte, bc.fixedChunkSize)
	copy(paddedPart, part)

	// Write the padded part to the file
	_, err = bc.cashFile.WriteAt(paddedPart, offset)
	if err != nil {
		return err
	}

	timestamp := time.Now().Unix()
	meta := chunkMetadata{
		LocationID: locationID,
		ChunkIndex: partIndex,
		Offset:     offset,
		Size:       int64(len(part)), // Store the actual size of the part, not the padded size
		Timestamp:  timestamp,        // Store the current timestamp as int64
	}

	// Update the metadata
	bc.metadata[locationID][chunkID] = append(bc.metadata[locationID][chunkID], meta)
	bc.cacheSize += bc.fixedChunkSize

	// Add to LRU queue
	bc.addLRU(locationID, chunkID, timestamp)

	return nil
}

// Read a specific chunk from the binary cashFile
func (bc *BinaryCache) readChunk(locationID int64, chunkID int64) ([]byte, error) {
	bc.chunkLock.Lock()
	defer bc.chunkLock.Unlock()

	locationMetadata, exists := bc.metadata[locationID]
	if !exists {
		return nil, fmt.Errorf("location ID %d not found", locationID)
	}

	chunkMetadata, exists := locationMetadata[chunkID]
	if !exists {
		return nil, fmt.Errorf("chunk %d not found for location ID %d", chunkID, locationID)
	}

	// Combine all parts
	var chunk []byte
	for _, meta := range chunkMetadata {
		part, err := bc.readChunkPart(meta)
		if err != nil {
			return nil, err
		}
		chunk = append(chunk, part...)
	}

	// Update the timestamp for LRU
	timestamp := time.Now().Unix()
	for _, meta := range chunkMetadata {
		meta.SetTimestamp(time.Now())
	}

	// Update the LRU queue
	bc.updateLRU(locationID, chunkID, timestamp)

	return chunk, nil
}

// Helper method to read a part of the chunk
func (bc *BinaryCache) readChunkPart(meta chunkMetadata) ([]byte, error) {
	// Seek to the chunk's offset
	_, err := bc.cashFile.Seek(meta.Offset, os.SEEK_SET)
	if err != nil {
		return nil, err
	}

	// Read the chunk's data
	paddedPart := make([]byte, bc.fixedChunkSize)
	_, err = bc.cashFile.Read(paddedPart)
	if err != nil {
		return nil, err
	}

	// Return only the actual size of the data, trimming any padding
	return paddedPart[:meta.Size], nil
}

// Add a chunk to the LRU queue
func (bc *BinaryCache) addLRU(locationID int64, chunkID int64, timestamp int64) {
	item := &LRUItem{
		locationID: locationID,
		chunkID:    chunkID,
		timestamp:  timestamp,
	}
	heap.Push(bc.lruQueue, item)
}

// Update a chunk's position in the LRU queue
func (bc *BinaryCache) updateLRU(locationID int64, chunkID int64, timestamp int64) {
	for _, item := range *bc.lruQueue {
		if item.locationID == locationID && item.chunkID == chunkID {
			bc.lruQueue.update(item, timestamp)
			return
		}
	}
}

// Evict chunks until the cache size is within the limit
func (bc *BinaryCache) evictIfNeeded() {
	for bc.cacheSize >= bc.maxCacheSize && bc.lruQueue.Len() > 0 { // Changed from '>' to '>='

		// Evict the least recently used chunk
		item := heap.Pop(bc.lruQueue).(*LRUItem)
		metas := bc.metadata[item.locationID][item.chunkID]
		for _, meta := range metas {
			bc.evictionList = append(bc.evictionList, &meta) // Add to the list of evicted chunks
			bc.cacheSize -= bc.fixedChunkSize
		}
		delete(bc.metadata[item.locationID], item.chunkID)
		if len(bc.metadata[item.locationID]) == 0 {
			delete(bc.metadata, item.locationID)
		}
	}
}

// Save metadata to the metadata cashFile
func (bc *BinaryCache) saveMetadata() error {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	_, err := bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	// Clear the metadata cashFile before saving new data
	err = bc.metadataFile.Truncate(0)
	if err != nil {
		return err
	}

	totalChunks := int64(0)
	for _, locationChunks := range bc.metadata {
		totalChunks += int64(len(locationChunks))
	}

	err = binary.Write(bc.metadataFile, binary.LittleEndian, totalChunks)
	if err != nil {
		return err
	}

	for locationID, locationChunks := range bc.metadata {
		for chunkID, metas := range locationChunks {
			for _, meta := range metas {
				err := binary.Write(bc.metadataFile, binary.LittleEndian, locationID)
				if err != nil {
					return err
				}
				err = binary.Write(bc.metadataFile, binary.LittleEndian, chunkID)
				if err != nil {
					return err
				}
				err = binary.Write(bc.metadataFile, binary.LittleEndian, meta.LocationID)
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

// Load metadata from the metadata cashFile
func (bc *BinaryCache) loadMetadata() error {
	bc.metadataLock.Lock()
	defer bc.metadataLock.Unlock()

	// Get the metadata cashFile size
	fileInfo, err := bc.metadataFile.Stat()
	if err != nil {
		return err
	}
	fileSize := fileInfo.Size()

	// Check if the metadata cashFile is empty or corrupted
	if fileSize == 0 {
		return bc.initializeFile()
	}

	_, err = bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	// Read number of chunks
	var numChunks int64
	err = binary.Read(bc.metadataFile, binary.LittleEndian, &numChunks)
	if err != nil {
		return bc.initializeFile()
	}

	for i := int64(0); i < numChunks; i++ {
		var locationID int64
		var chunkID int64
		var meta chunkMetadata

		err = binary.Read(bc.metadataFile, binary.LittleEndian, &locationID)
		if err != nil {
			if err == io.EOF {
				break // Gracefully handle unexpected EOF
			}
			return err
		}
		err = binary.Read(bc.metadataFile, binary.LittleEndian, &chunkID)
		if err != nil {
			if err == io.EOF {
				break // Gracefully handle unexpected EOF
			}
			return err
		}
		err = binary.Read(bc.metadataFile, binary.LittleEndian, &meta.LocationID)
		if err != nil {
			if err == io.EOF {
				break // Gracefully handle unexpected EOF
			}
			return err
		}
		err = binary.Read(bc.metadataFile, binary.LittleEndian, &meta.ChunkIndex)
		if err != nil {
			if err == io.EOF {
				break // Gracefully handle unexpected EOF
			}
			return err
		}
		err = binary.Read(bc.metadataFile, binary.LittleEndian, &meta.Offset)
		if err != nil {
			if err == io.EOF {
				break // Gracefully handle unexpected EOF
			}
			return err
		}
		err = binary.Read(bc.metadataFile, binary.LittleEndian, &meta.Size)
		if err != nil {
			if err == io.EOF {
				break // Gracefully handle unexpected EOF
			}
			return err
		}
		err = binary.Read(bc.metadataFile, binary.LittleEndian, &meta.Timestamp)
		if err != nil {
			if err == io.EOF {
				break // Gracefully handle unexpected EOF
			}
			return err
		}

		if _, exists := bc.metadata[locationID]; !exists {
			bc.metadata[locationID] = make(map[int64][]chunkMetadata)
		}

		bc.metadata[locationID][chunkID] = append(bc.metadata[locationID][chunkID], meta)
		bc.cacheSize += bc.fixedChunkSize

		// Add the chunk to the LRU queue
		bc.addLRU(locationID, chunkID, meta.Timestamp)
	}

	return nil
}

// Initialize the metadata cashFile
func (bc *BinaryCache) initializeFile() error {
	// Truncate the metadata cashFile to clear existing data
	err := bc.metadataFile.Truncate(0)
	if err != nil {
		return err
	}

	_, err = bc.metadataFile.Seek(0, os.SEEK_SET)
	if err != nil {
		return err
	}

	// Initialize with zero chunks
	var numChunks int64 = 0
	err = binary.Write(bc.metadataFile, binary.LittleEndian, numChunks)
	if err != nil {
		return err
	}

	// Reset in-memory metadata
	bc.metadata = make(map[int64]map[int64][]chunkMetadata)
	bc.cacheSize = 0

	// Ensure changes are written to disk
	err = bc.metadataFile.Sync()
	if err != nil {
		return err
	}

	return nil
}
