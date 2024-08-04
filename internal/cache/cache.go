package cache

import (
	"bytes"
	"encoding/gob"
	"sync"
	"webBridgeBot/internal/types"

	"github.com/coocood/freecache"
	"github.com/gotd/td/tg"
)

var cache *Cache

type Cache struct {
	cache *freecache.Cache
	mu    sync.RWMutex
}

func init() {
	gob.Register(types.DocumentFile{})
	gob.Register(tg.InputDocumentFileLocation{})
	cache = &Cache{cache: freecache.NewCache(10 * 1024 * 1024)}
}

func GetCache() *Cache {
	return cache
}

func (c *Cache) Get(key string, value *types.DocumentFile) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	data, err := cache.cache.Get([]byte(key))
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
	cache.cache.Set([]byte(key), buf.Bytes(), expireSeconds)
	return nil
}

func (c *Cache) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cache.cache.Del([]byte(key))
	return nil
}
