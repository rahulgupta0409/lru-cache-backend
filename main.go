package main

import (
	"container/list"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

type cacheValue struct {
	data interface{}
	link *list.Element
}

type lruQueueItem struct {
	key interface{}
	ttl time.Time
}

type ConfigBuilder struct {
	defaultTTL     time.Duration
	maxSize        int
	cleanInterval  time.Duration
	deleteCallback func(count int64)
}

func Configuration() *ConfigBuilder {
	return &ConfigBuilder{
		defaultTTL:     -1,
		maxSize:        math.MaxInt32 - 1,
		cleanInterval:  time.Minute,
		deleteCallback: nil,
	}
}

func (b *ConfigBuilder) SetDefaultTTL(ttl time.Duration) *ConfigBuilder {
	b.defaultTTL = ttl
	return b
}

func (b *ConfigBuilder) SetMaxSize(size int) *ConfigBuilder {
	b.maxSize = size
	return b
}

func (b *ConfigBuilder) SetCleanupInterval(interval time.Duration) *ConfigBuilder {
	b.cleanInterval = interval
	return b
}

func (b *ConfigBuilder) SetDeleteCallback(callback func(count int64)) *ConfigBuilder {
	b.deleteCallback = callback
	return b
}

type LRUCache struct {
	values map[interface{}]*cacheValue
	queue  *list.List

	maxSize        int
	defaultTTL     time.Duration
	deleteCallback func(count int64)

	lock sync.RWMutex
}

func NewLRUCache(config *ConfigBuilder) *LRUCache {
	cache := &LRUCache{
		values: make(map[interface{}]*cacheValue),
		queue:  list.New(),

		// Configuration
		maxSize:        config.maxSize,
		defaultTTL:     config.defaultTTL,
		deleteCallback: config.deleteCallback,
	}

	// Package agreement: there is no way to stop this goroutine. Reuse cache if possible.
	// Avoid multiple create/delete cycles
	if config.defaultTTL >= 0 {
		go func() {
			for {
				<-time.After(config.cleanInterval)
				cache.cleanInterval()
			}
		}()
	}

	return cache
}

func (c *LRUCache) Get(key interface{}) (interface{}, bool) {
	c.lock.RLock()
	value, found := c.values[key]
	c.lock.RUnlock()
	if !found {
		return nil, false
	}

	c.lock.Lock()
	c.queue.MoveToFront(value.link)
	c.lock.Unlock()

	return value.data, true
}

func (c *LRUCache) Set(key interface{}, value interface{}) {
	c.lock.Lock()
	_, found := c.values[key]
	if found {
		c.values[key].data = value
		c.queue.MoveToFront(c.values[key].link)
		c.lock.Unlock()
		return
	}
	c.lock.Unlock()

	LRUitem := &lruQueueItem{
		key: key,
		ttl: time.Now().Add(c.defaultTTL),
	}
	c.lock.Lock()
	queueItem := c.queue.PushFront(LRUitem)
	c.values[key] = &cacheValue{
		data: value,
		link: queueItem,
	}
	if c.queue.Len() > c.maxSize {
		item := c.queue.Back().Value.(*lruQueueItem)
		cachedItem := c.values[item.key]
		c.unsafeDelete(item.key, cachedItem)
	}
	c.lock.Unlock()
}

func (c *LRUCache) Delete(key interface{}) {
	c.lock.RLock()
	value, found := c.values[key]
	c.lock.RUnlock()

	if found {
		c.lock.Lock()
		c.unsafeDelete(key, value)
		c.lock.Unlock()
		if c.deleteCallback != nil {
			c.deleteCallback(1)
		}
	}
}

func (c *LRUCache) Clean() {
	// TODO: Check if GOCG cleans the dropped values and do not do a memory leaking
	c.lock.Lock()
	c.values = make(map[interface{}]*cacheValue)
	c.queue = list.New()
	c.lock.Unlock()
}

func (c *LRUCache) Size() int {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.queue.Len()
}

// Cleans up the expired items. Do not set the clean interval too low to avoid CPU load
func (c *LRUCache) cleanInterval() {
	var deleted int64
	c.lock.Lock()
	for key, value := range c.values {
		item := value.link.Value.(*lruQueueItem)
		if item.ttl.Sub(time.Now()) < 0 {
			c.unsafeDelete(key, value)
			deleted++
		}
	}
	c.lock.Unlock()

	if c.deleteCallback != nil {
		c.deleteCallback(deleted)
	}
}

func (c *LRUCache) unsafeDelete(key interface{}, value *cacheValue) {
	c.queue.Remove(value.link)
	delete(c.values, key)
	value = nil
}

func respondWithJson(w http.ResponseWriter, code int, payload interface{}) interface{} {
	response, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
	return payload
}

func HandlerSet(c *LRUCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var data struct {
			Key   interface{} `json:"key"`
			Value interface{} `json:"value"`
		}
		err := json.NewDecoder(r.Body).Decode(&data)
		if err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		c.Set(data.Key, data.Value)
		respondWithJson(w, http.StatusCreated, data)

	}
}

func HandlerGet(c *LRUCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")

		if key == "" {
			http.Error(w, "Key not provided", http.StatusBadRequest)
			return
		}
		data, _ := c.Get(key)
		respondWithJson(w, http.StatusCreated, data)
	}
}

func main() {
	config := Configuration()
	config.SetDefaultTTL(5 * time.Second)
	config.SetMaxSize(1024)
	cache := NewLRUCache(config)
	router := mux.NewRouter()

	router.HandleFunc("/getValue", HandlerGet(cache)).Methods("GET")
	router.HandleFunc("/setValue", HandlerSet(cache)).Methods("POST")
	http.Handle("/", router)

	port := 8080
	fmt.Printf("Server listening on :%d...\n", port)
	http.ListenAndServe(fmt.Sprintf(":%d", port), router)
}
