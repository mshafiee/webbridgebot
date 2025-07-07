package cache

import (
	"bytes"
	"encoding/gob"
	"sync"
	"webBridgeBot/internal/types"

	"github.com/coocood/freecache"
	"github.com/gotd/td/tg"
)

// Define the Cache struct explicitly
type Cache struct {
	cache *freecache.Cache
	mu    sync.RWMutex
}

var cache *Cache

func init() {
	gob.Register(types.DocumentFile{})
	// Register the concrete types that implement tg.InputFileLocationClass
	gob.Register(&tg.InputDocumentFileLocation{})
	gob.Register(&tg.InputPhotoFileLocation{})
	cache = &Cache{cache: freecache.NewCache(10 * 1024 * 1024)} // 10MB default cache size
}

func GetCache() *Cache {
	return cache
}

func (c *Cache) Get(key string, value *types.DocumentFile) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	data, err := c.cache.Get([]byte(key)) // Use c.cache here
	if err != nil {
		return err
	}
	dec := gob.NewDecoder(bytes.NewReader(data))
	err = dec.Decode(&value)
	if err != nil {
		return err
	}
	return nil
}

func (c *Cache) Set(key string, value *types.DocumentFile, expireSeconds int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	err := enc.Encode(value)
	if err != nil {
		return err
	}
	c.cache.Set([]byte(key), buf.Bytes(), expireSeconds) // Use c.cache here
	return nil
}

func (c *Cache) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache.Del([]byte(key)) // Use c.cache here
	return nil
}
