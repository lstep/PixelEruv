package main

import "sync"

// FlagCache memoizes the result of looking up a flag code for a given IP
// address, so the (future) IP->country resolution runs at most once per IP.
//
// Lookup currently always returns "neutral" — a placeholder flag code rendered
// by the template as a neutral flag. The real GeoIP lookup will be wired in
// later; until then the cache is a no-op seam that preserves the call shape.
type FlagCache struct {
	mu    sync.RWMutex
	cache map[string]string
}

// NewFlagCache returns an empty FlagCache.
func NewFlagCache() *FlagCache {
	return &FlagCache{cache: make(map[string]string)}
}

// Lookup returns the flag code for the given IP. Results are cached so the
// resolution is computed at most once per IP per process lifetime.
func (f *FlagCache) Lookup(ip string) string {
	if ip == "" {
		return "neutral"
	}
	f.mu.RLock()
	if code, ok := f.cache[ip]; ok {
		f.mu.RUnlock()
		return code
	}
	f.mu.RUnlock()

	code := f.resolve(ip)

	f.mu.Lock()
	if existing, ok := f.cache[ip]; ok {
		// Another goroutine won the race; keep the first result.
		f.mu.Unlock()
		return existing
	}
	f.cache[ip] = code
	f.mu.Unlock()
	return code
}

// resolve maps an IP address to a flag code. Placeholder: always returns
// "neutral". To be replaced with a real GeoIP lookup (e.g. MaxMind DB or
// an IP-geolocation service) returning a lowercase ISO 3166-1 alpha-2
// country code.
func (f *FlagCache) resolve(ip string) string {
	return "neutral"
}

// flagCache is the process-wide FlagCache used by the flagFor template func.
var flagCache = NewFlagCache()
