package geoip

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"
)

// Region codes
const (
	RegionJP    = "jp"
	RegionKR    = "kr"
	RegionUS    = "us"
	RegionHK    = "hk"
	RegionTW    = "tw"
	RegionOther = "other"
)

// Default GeoIP database download URL
const (
	DefaultGeoIPURL = "https://ghproxy.net/https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-Country.mmdb"
)

// RegionInfo contains region details
type RegionInfo struct {
	Code    string // "jp", "kr", "us", "hk", "tw", "other"
	Country string // Full country name
	ISOCode string // ISO country code
}

// Lookup provides GeoIP lookup functionality
type Lookup struct {
	db             *geoip2.Reader
	mu             sync.RWMutex
	path           string
	updateInterval time.Duration
	stopChan       chan struct{}
	updateOnce     sync.Once
}

// EnsureDatabase checks if the GeoIP database exists, and downloads it if not
func EnsureDatabase(dbPath string) error {
	if dbPath == "" {
		return nil
	}

	// Check if file already exists and is valid
	info, err := os.Stat(dbPath)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("geoip database path is not a file: %s", dbPath)
		}
		if info.Size() > 0 {
			return nil // File exists and has content
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat geoip database: %w", err)
	}

	log.Printf("📥 GeoIP database not found at %s, downloading...", dbPath)
	if err := fetchDatabase(dbPath); err != nil {
		return err
	}
	log.Printf("✅ GeoIP database downloaded successfully to %s", dbPath)
	return nil
}

// DownloadDatabase downloads the GeoIP database to dbPath, overwriting any
// existing file. Use this to (re)download the IP library on demand from the
// WebUI regardless of whether a file already exists.
func DownloadDatabase(dbPath string) error {
	if dbPath == "" {
		return fmt.Errorf("geoip database path is empty")
	}
	log.Printf("📥 GeoIP database download requested for %s", dbPath)
	if err := fetchDatabase(dbPath); err != nil {
		return err
	}
	log.Printf("✅ GeoIP database downloaded successfully to %s", dbPath)
	return nil
}

// fetchDatabase downloads the GeoIP database from DefaultGeoIPURL into a
// temporary file in the same directory as dbPath, validates it, and atomically
// renames it to dbPath, overwriting any existing file. The temp file lives in
// the same directory so the rename is atomic on the same filesystem.
func fetchDatabase(dbPath string) error {
	// Create parent directory if needed
	dir := filepath.Dir(dbPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}

	// Download with timeout
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, DefaultGeoIPURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: unexpected status %s", resp.Status)
	}

	// Download to temporary file
	tempFile, err := os.CreateTemp(dir, ".geoip-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if tempFile != nil {
			tempFile.Close()
		}
		if cleanup {
			os.Remove(tempPath)
		}
	}()

	// Copy with progress tracking
	progress := &progressWriter{total: resp.ContentLength}
	reader := io.TeeReader(resp.Body, progress)
	written, err := io.Copy(tempFile, reader)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	// Verify download completeness
	if resp.ContentLength > 0 && written < resp.ContentLength {
		return fmt.Errorf("incomplete download (%d/%d bytes)", written, resp.ContentLength)
	}

	// Sync and close temp file
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	tempFile = nil

	// Validate MMDB format
	if err := validateMMDB(tempPath); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, dbPath); err != nil {
		return fmt.Errorf("rename failed: %w", err)
	}
	cleanup = false
	return nil
}

// DatabaseStatusInfo describes the on-disk GeoIP database file.
type DatabaseStatusInfo struct {
	DatabasePath string `json:"database_path"`
	Exists       bool   `json:"exists"`
	SizeBytes    int64  `json:"size_bytes"`
	ModifiedAt   string `json:"modified_at"` // RFC3339 (UTC), empty if the file does not exist
	DownloadURL  string `json:"download_url"`
}

// DatabaseStatus reports the on-disk state of the GeoIP database at dbPath.
func DatabaseStatus(dbPath string) DatabaseStatusInfo {
	info := DatabaseStatusInfo{
		DatabasePath: dbPath,
		DownloadURL:  DefaultGeoIPURL,
	}
	if dbPath == "" {
		return info
	}
	st, err := os.Stat(dbPath)
	if err != nil {
		return info // exists stays false
	}
	info.Exists = true
	info.SizeBytes = st.Size()
	info.ModifiedAt = st.ModTime().UTC().Format(time.RFC3339)
	return info
}

// progressWriter tracks download progress
type progressWriter struct {
	total       int64
	downloaded  int64
	lastPercent int64
	lastLog     time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.downloaded += int64(n)

	now := time.Now()
	if p.total > 0 {
		percent := p.downloaded * 100 / p.total
		if percent >= 100 || percent >= p.lastPercent+10 || now.Sub(p.lastLog) >= 3*time.Second {
			log.Printf("   Progress: %d%% (%d/%d bytes)", percent, p.downloaded, p.total)
			p.lastPercent = percent
			p.lastLog = now
		}
	} else if now.Sub(p.lastLog) >= 3*time.Second {
		log.Printf("   Downloaded: %d bytes", p.downloaded)
		p.lastLog = now
	}

	return n, nil
}

// validateMMDB performs basic validation of MMDB file format
func validateMMDB(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 1024 {
		return fmt.Errorf("file too small (%d bytes)", info.Size())
	}

	// Check for MaxMind metadata in the last 8KB
	const tailSize int64 = 8192
	readSize := tailSize
	if info.Size() < readSize {
		readSize = info.Size()
	}
	if _, err := file.Seek(-readSize, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, readSize)
	if _, err := io.ReadFull(file, buf); err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	if !bytes.Contains(buf, []byte("MaxMind.com")) {
		return fmt.Errorf("missing MaxMind metadata")
	}

	return nil
}

// downloadFile downloads a file from URL to the specified path
func downloadFile(filepath string, url string) error {
	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	// Writer the body to file
	size, err := io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	log.Printf("   Downloaded %d bytes", size)
	return nil
}

// New creates a new GeoIP lookup instance
func New(dbPath string) (*Lookup, error) {
	return NewWithAutoUpdate(dbPath, 0)
}

// NewWithAutoUpdate creates a new GeoIP lookup instance with auto-update support
func NewWithAutoUpdate(dbPath string, updateInterval time.Duration) (*Lookup, error) {
	if dbPath == "" {
		return &Lookup{}, nil
	}

	// Ensure database exists (download if needed)
	if err := EnsureDatabase(dbPath); err != nil {
		return nil, fmt.Errorf("ensure database: %w", err)
	}

	db, err := geoip2.Open(dbPath)
	if err != nil {
		return nil, err
	}

	lookup := &Lookup{
		db:             db,
		path:           dbPath,
		updateInterval: updateInterval,
		stopChan:       make(chan struct{}),
	}

	// Start auto-update goroutine if interval is set
	if updateInterval > 0 {
		go lookup.autoUpdateLoop()
		log.Printf("🔄 GeoIP auto-update enabled (interval: %v)", updateInterval)
	}

	return lookup, nil
}

// autoUpdateLoop periodically updates the GeoIP database
func (l *Lookup) autoUpdateLoop() {
	ticker := time.NewTicker(l.updateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := l.Update(); err != nil {
				log.Printf("⚠️  GeoIP auto-update failed: %v", err)
			}
		case <-l.stopChan:
			return
		}
	}
}

// Update downloads and reloads the GeoIP database
func (l *Lookup) Update() error {
	log.Printf("🔄 Updating GeoIP database...")

	// Download to temporary file
	tempPath := l.path + ".update"
	if err := downloadDatabase(tempPath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer os.Remove(tempPath) // Clean up temp file

	// Validate the downloaded database
	if err := validateMMDB(tempPath); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Open new database
	newDB, err := geoip2.Open(tempPath)
	if err != nil {
		return fmt.Errorf("open new database: %w", err)
	}

	// Hot-swap the database
	l.mu.Lock()
	oldDB := l.db
	l.db = newDB
	l.mu.Unlock()

	// Close old database
	if oldDB != nil {
		oldDB.Close()
	}

	// Replace the old file with new one
	if err := os.Rename(tempPath, l.path); err != nil {
		log.Printf("⚠️  Failed to replace database file: %v (using in-memory version)", err)
	}

	log.Printf("✅ GeoIP database updated successfully")
	return nil
}

// downloadDatabase downloads the GeoIP database to the specified path
func downloadDatabase(filepath string) error {
	// Create parent directory if needed
	dir := filepath[:strings.LastIndex(filepath, "/")]
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}

	// Download with timeout
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, DefaultGeoIPURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	// Create temp file
	tempFile, err := os.CreateTemp(dir, ".geoip-download-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if tempFile != nil {
			tempFile.Close()
		}
		if cleanup {
			os.Remove(tempPath)
		}
	}()

	// Copy with progress tracking
	progress := &progressWriter{total: resp.ContentLength}
	reader := io.TeeReader(resp.Body, progress)
	written, err := io.Copy(tempFile, reader)
	if err != nil {
		return err
	}

	// Verify download completeness
	if resp.ContentLength > 0 && written < resp.ContentLength {
		return fmt.Errorf("incomplete download (%d/%d bytes)", written, resp.ContentLength)
	}

	// Sync and close
	if err := tempFile.Sync(); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	tempFile = nil

	// Rename to target path
	if err := os.Rename(tempPath, filepath); err != nil {
		return err
	}
	cleanup = false

	return nil
}

// Close closes the GeoIP database and stops auto-update
func (l *Lookup) Close() error {
	// Stop auto-update goroutine
	l.updateOnce.Do(func() {
		if l.stopChan != nil {
			close(l.stopChan)
		}
	})

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db != nil {
		return l.db.Close()
	}
	return nil
}

// IsEnabled returns true if GeoIP lookup is available
func (l *Lookup) IsEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.db != nil
}

// LookupIP returns region info for an IP address
func (l *Lookup) LookupIP(ipStr string) RegionInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.db == nil {
		return RegionInfo{Code: RegionOther, Country: "Unknown", ISOCode: ""}
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return RegionInfo{Code: RegionOther, Country: "Unknown", ISOCode: ""}
	}

	record, err := l.db.Country(ip)
	if err != nil {
		return RegionInfo{Code: RegionOther, Country: "Unknown", ISOCode: ""}
	}

	isoCode := record.Country.IsoCode
	country := record.Country.Names["en"]
	if country == "" {
		country = isoCode
	}

	return RegionInfo{
		Code:    isoCodeToRegion(isoCode),
		Country: country,
		ISOCode: isoCode,
	}
}

// LookupURI extracts server from URI and returns region info
func (l *Lookup) LookupURI(uri string) RegionInfo {
	host := extractHostFromURI(uri)
	if host == "" {
		return RegionInfo{Code: RegionOther, Country: "Unknown", ISOCode: ""}
	}

	// Resolve hostname to IP if needed
	ip := net.ParseIP(host)
	if ip == nil {
		// It's a hostname, try to resolve
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return RegionInfo{Code: RegionOther, Country: "Unknown", ISOCode: ""}
		}
		host = ips[0].String()
	}

	return l.LookupIP(host)
}

// extractHostFromURI extracts the host/IP from various proxy URI formats
func extractHostFromURI(uri string) string {
	// Handle different URI schemes
	lowerURI := strings.ToLower(uri)

	// Standard URL formats, including HTTP and SOCKS upstream proxies.
	if strings.HasPrefix(lowerURI, "vmess://") ||
		strings.HasPrefix(lowerURI, "vless://") ||
		strings.HasPrefix(lowerURI, "trojan://") ||
		strings.HasPrefix(lowerURI, "hysteria://") ||
		strings.HasPrefix(lowerURI, "hysteria2://") ||
		strings.HasPrefix(lowerURI, "hy2://") ||
		strings.HasPrefix(lowerURI, "anytls://") ||
		strings.HasPrefix(lowerURI, "http://") ||
		strings.HasPrefix(lowerURI, "socks5://") {
		// Standard URL format: scheme://user@host:port?params#fragment
		parsed, err := url.Parse(uri)
		if err != nil {
			return ""
		}
		return parsed.Hostname()
	}

	if strings.HasPrefix(lowerURI, "ss://") {
		// Shadowsocks format: ss://base64(method:password)@host:port#name
		// or ss://base64@host:port#name
		return extractSSHost(uri)
	}

	if strings.HasPrefix(lowerURI, "ssr://") {
		// SSR format is base64 encoded
		return extractSSRHost(uri)
	}

	return ""
}

func extractSSHost(uri string) string {
	// Remove ss:// prefix
	content := strings.TrimPrefix(uri, "ss://")

	// Remove fragment (#name)
	if idx := strings.Index(content, "#"); idx != -1 {
		content = content[:idx]
	}

	// Check if it's the new format: base64@host:port
	if atIdx := strings.LastIndex(content, "@"); atIdx != -1 {
		hostPort := content[atIdx+1:]
		if colonIdx := strings.LastIndex(hostPort, ":"); colonIdx != -1 {
			return hostPort[:colonIdx]
		}
		return hostPort
	}

	// Old format: entire content is base64
	return ""
}

func extractSSRHost(uri string) string {
	// SSR is complex, skip for now - will be marked as "other"
	return ""
}

// isoCodeToRegion maps an ISO 3166-1 alpha-2 country code to a region code.
//
// The region code is simply the lowercased country code (e.g. "JP" -> "jp",
// "SG" -> "sg"), so statistics and routing reflect the actual country rather
// than collapsing most of the world into "other". An empty code (unknown /
// unresolvable host) falls back to RegionOther.
func isoCodeToRegion(isoCode string) string {
	code := strings.ToLower(strings.TrimSpace(isoCode))
	if code == "" {
		return RegionOther
	}
	return code
}

// AllRegions returns the well-known region codes. Region codes are not limited
// to this set at runtime — any lowercased ISO country code may appear — but
// these are the ones treated as first-class for routing and display.
func AllRegions() []string {
	return []string{RegionJP, RegionKR, RegionUS, RegionHK, RegionTW, RegionOther}
}

// RegionName returns the display name for a region code.
func RegionName(code string) string {
	switch code {
	case RegionJP:
		return "Japan"
	case RegionKR:
		return "Korea"
	case RegionUS:
		return "USA"
	case RegionHK:
		return "Hong Kong"
	case RegionTW:
		return "Taiwan"
	case RegionOther:
		return "Other"
	default:
		// Region codes are lowercased ISO country codes; surface the code
		// uppercased rather than a generic "Unknown".
		upper := strings.ToUpper(code)
		if len(upper) == 2 && isAlpha(upper) {
			return upper
		}
		return "Unknown"
	}
}

// RegionEmoji returns the flag emoji for a region code.
//
// For any 2-letter ISO country code the flag is computed from the regional
// indicator symbols, so every country is supported without a hardcoded table.
func RegionEmoji(code string) string {
	if code == RegionOther {
		return "🌍"
	}
	c := strings.ToLower(code)
	if len(c) != 2 || !isAlpha(c) {
		return "❓"
	}
	const base = 0x1f1e6 // regional indicator symbol A
	return string([]rune{rune(c[0]) - 'a' + base, rune(c[1]) - 'a' + base})
}

// isAlpha reports whether s consists of two ASCII letters (a-z, A-Z).
func isAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return len(s) == 2
}
