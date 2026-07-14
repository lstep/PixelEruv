package main

import (
	"net"
	"os"
	"strings"
	"sync"

	"github.com/oschwald/maxminddb-golang"
)

// iplocateRecord is the record layout used by the iplocate ip-to-country
// MMDB. Unlike MaxMind's nested structure (country.iso_code), iplocate uses
// a flat layout with a top-level country_code field.
type iplocateRecord struct {
	CountryCode string `maxminddb:"country_code"`
	CountryName string `maxminddb:"country_name"`
}

// FlagCache memoizes the result of looking up a flag code for a given IP
// address, so the GeoIP resolution runs at most once per IP per process
// lifetime.
//
// The underlying MMDB reader is opened once at startup (NewFlagCache) and
// closed via Close. If no database is available, Lookup returns "neutral"
// for every IP — the template renders a neutral placeholder flag.
type FlagCache struct {
	mu     sync.RWMutex
	cache  map[string]string
	reader *maxminddb.Reader
}

// NewFlagCache opens the GeoIP MMDB at dbPath. If dbPath is empty or the
// file does not exist, a no-reader cache is returned (Lookup will always
// return "neutral"). This is non-fatal: the audit service stays up and
// shows neutral flags.
func NewFlagCache(dbPath string) (*FlagCache, error) {
	fc := &FlagCache{cache: make(map[string]string)}
	if dbPath == "" {
		return fc, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return fc, nil
	}
	reader, err := maxminddb.Open(dbPath)
	if err != nil {
		return fc, nil
	}
	fc.reader = reader
	return fc, nil
}

// Close releases the MMDB reader if one is open.
func (f *FlagCache) Close() {
	if f.reader != nil {
		f.reader.Close()
	}
}

// Lookup returns the flag code for the given IP. Results are cached so the
// GeoIP resolution is computed at most once per IP per process lifetime.
// Returns "neutral" for empty IPs, invalid IPs, or when no MMDB is loaded.
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

// resolve maps an IP address to a lowercase ISO 3166-1 alpha-2 country code
// via the MMDB reader. Returns "neutral" if the reader is nil, the IP is
// invalid, or the country code is empty.
func (f *FlagCache) resolve(ip string) string {
	if f.reader == nil {
		return "neutral"
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "neutral"
	}
	var record iplocateRecord
	if err := f.reader.Lookup(parsed, &record); err != nil {
		return "neutral"
	}
	if record.CountryCode == "" {
		return "neutral"
	}
	return strings.ToLower(record.CountryCode)
}

// flagCache is the process-wide FlagCache used by the flagClass template
// func. Initialized in main.go.
var flagCache *FlagCache

// flagClassFor returns the CSS class string for the flag span of the given
// IP. Real country codes render as "fi fi-<cc>" (lipis/flag-icons classes);
// the neutral fallback renders as "flag flag-neutral".
func flagClassFor(ip string) string {
	code := "neutral"
	if flagCache != nil {
		code = flagCache.Lookup(ip)
	}
	if code == "neutral" {
		return "flag flag-neutral"
	}
	return "fi fi-" + code
}
