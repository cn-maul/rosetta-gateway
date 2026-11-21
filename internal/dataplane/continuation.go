package dataplane

import (
	"sync"
	"time"
)

const (
	continuationTTL      = 24 * time.Hour
	continuationCapacity = 8192
)

type continuationKey struct {
	userID, keyID, publicModel, responseID string
}

type continuationBinding struct {
	targetID, providerID, endpoint, modelID, credentialID string
	credentialVersion                                     [32]byte
	expires                                               time.Time
}

type continuationIndex struct {
	mu      sync.Mutex
	entries map[continuationKey]continuationBinding
}

func (index *continuationIndex) get(key continuationKey) (continuationBinding, bool) {
	index.mu.Lock()
	defer index.mu.Unlock()
	binding, found := index.entries[key]
	if found && !time.Now().Before(binding.expires) {
		delete(index.entries, key)
		return continuationBinding{}, false
	}
	return binding, found
}

func (index *continuationIndex) put(key continuationKey, binding continuationBinding) {
	if key.responseID == "" || key.keyID == "" || binding.credentialID == "" {
		return
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.entries == nil {
		index.entries = make(map[continuationKey]continuationBinding)
	}
	now := time.Now()
	if _, found := index.entries[key]; !found && len(index.entries) >= continuationCapacity {
		var oldestKey continuationKey
		var oldest time.Time
		for candidate, entry := range index.entries {
			if !now.Before(entry.expires) {
				delete(index.entries, candidate)
			} else if oldest.IsZero() || entry.expires.Before(oldest) {
				oldestKey, oldest = candidate, entry.expires
			}
		}
		if len(index.entries) >= continuationCapacity {
			delete(index.entries, oldestKey)
		}
	}
	binding.expires = now.Add(continuationTTL)
	index.entries[key] = binding
}
