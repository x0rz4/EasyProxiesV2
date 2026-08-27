package geoip

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oschwald/geoip2-golang/v2"
)

func withDownloadTestConfig(t *testing.T, urls []string, validator func(string) error) {
	t.Helper()
	oldURLs, oldClient, oldValidator := downloadURLs, downloadHTTPClient, databaseValidator
	downloadURLs = urls
	downloadHTTPClient = &http.Client{}
	databaseValidator = validator
	downloadSourceMu.Lock()
	oldSource := lastSuccessfulDownload
	lastSuccessfulDownload = ""
	downloadSourceMu.Unlock()
	t.Cleanup(func() {
		downloadURLs, downloadHTTPClient, databaseValidator = oldURLs, oldClient, oldValidator
		downloadSourceMu.Lock()
		lastSuccessfulDownload = oldSource
		downloadSourceMu.Unlock()
	})
}

func markerValidator(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "valid-mmdb") {
		return errors.New("invalid fixture")
	}
	return nil
}

func TestDownloadDatabaseFallsBackToSecondDirectSource(t *testing.T) {
	var requests []string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests = append(requests, "primary")
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests = append(requests, "fallback")
		_, _ = w.Write([]byte(strings.Repeat("valid-mmdb", 200)))
	}))
	defer fallback.Close()
	withDownloadTestConfig(t, []string{primary.URL, fallback.URL}, markerValidator)

	dbPath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	if err := DownloadDatabase(dbPath); err != nil {
		t.Fatal(err)
	}
	if strings.Join(requests, ",") != "primary,fallback" {
		t.Fatalf("source order = %v", requests)
	}
	status := DatabaseStatus(dbPath)
	if !status.Exists || status.SourceURL != fallback.URL {
		t.Fatalf("database status = %+v", status)
	}
}

func TestEnsureDatabaseReplacesInvalidFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("valid-mmdb", 200)))
	}))
	defer server.Close()
	withDownloadTestConfig(t, []string{server.URL}, markerValidator)

	dbPath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	if err := os.WriteFile(dbPath, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDatabase(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := markerValidator(dbPath); err != nil {
		t.Fatalf("invalid database was not replaced: %v", err)
	}
}

func TestDownloadDatabaseReportsEverySourceAndPreservesExistingFile(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "one", http.StatusBadGateway) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "two", http.StatusForbidden) }))
	defer second.Close()
	withDownloadTestConfig(t, []string{first.URL, second.URL}, markerValidator)

	dbPath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	if err := os.WriteFile(dbPath, []byte("keep-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := DownloadDatabase(dbPath)
	if err == nil || !strings.Contains(err.Error(), first.URL) || !strings.Contains(err.Error(), second.URL) {
		t.Fatalf("download error = %v", err)
	}
	data, readErr := os.ReadFile(dbPath)
	if readErr != nil || string(data) != "keep-me" {
		t.Fatalf("existing file changed: data=%q err=%v", data, readErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(dbPath), ".geoip-*.mmdb"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("temporary downloads were not cleaned up: matches=%v err=%v", matches, globErr)
	}
}

func TestValidateMMDBRejectsMarkerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.mmdb")
	data := []byte(strings.Repeat("not-a-database", 1000) + "MaxMind.com")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMMDB(path); err == nil {
		t.Fatal("marker-only file passed MMDB validation")
	}
}

func TestParseLookupAddress(t *testing.T) {
	for _, input := range []string{"203.0.113.10", "2001:db8::10"} {
		if _, ok := parseLookupAddress(input); !ok {
			t.Fatalf("parseLookupAddress(%q) rejected a valid address", input)
		}
	}
	if _, ok := parseLookupAddress("not-an-ip"); ok {
		t.Fatal("parseLookupAddress accepted an invalid address")
	}
}

func TestRegionInfoFromCountry(t *testing.T) {
	tests := []struct {
		name   string
		record *geoip2.Country
		want   RegionInfo
	}{
		{
			name: "localized country name",
			record: &geoip2.Country{Country: geoip2.CountryRecord{
				ISOCode: "JP",
				Names:   geoip2.Names{English: "Japan"},
			}},
			want: RegionInfo{Code: RegionJP, Country: "Japan", ISOCode: "JP"},
		},
		{
			name: "ISO code fallback",
			record: &geoip2.Country{Country: geoip2.CountryRecord{
				ISOCode: "SG",
			}},
			want: RegionInfo{Code: "sg", Country: "SG", ISOCode: "SG"},
		},
		{name: "nil record", want: unknownRegionInfo()},
		{name: "empty record", record: &geoip2.Country{}, want: unknownRegionInfo()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := regionInfoFromCountry(tt.record); got != tt.want {
				t.Fatalf("regionInfoFromCountry() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExtractHostFromURISupportsHTTPAndSOCKS5(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{name: "http", uri: "http://alice:secret@example.com:8080", want: "example.com"},
		{name: "socks5", uri: "socks5://alice:secret@99.144.123.135:30350", want: "99.144.123.135"},
	}

	for _, tt := range tests {
		if got := extractHostFromURI(tt.uri); got != tt.want {
			t.Fatalf("%s: extractHostFromURI(%q) = %q, want %q", tt.name, tt.uri, got, tt.want)
		}
	}
}

func TestIsoCodeToRegionIsCountryCode(t *testing.T) {
	// Well-known regions keep their codes.
	if got := isoCodeToRegion("JP"); got != "jp" {
		t.Errorf("isoCodeToRegion(JP) = %q, want jp", got)
	}
	if got := isoCodeToRegion("SG"); got != "sg" {
		t.Errorf("isoCodeToRegion(SG) = %q, want sg", got)
	}
	if got := isoCodeToRegion("DE"); got != "de" {
		t.Errorf("isoCodeToRegion(DE) = %q, want de", got)
	}
	// Unknown/empty falls back to "other".
	if got := isoCodeToRegion(""); got != RegionOther {
		t.Errorf("isoCodeToRegion(\"\") = %q, want %q", got, RegionOther)
	}
}

func TestRegionNameShowsCountryCode(t *testing.T) {
	// Well-known regions show friendly names.
	if got := RegionName(RegionJP); got != "Japan" {
		t.Errorf("RegionName(jp) = %q, want Japan", got)
	}
	// Any 2-letter code shows the uppercased code (not "Unknown").
	if got := RegionName("sg"); got != "SG" {
		t.Errorf("RegionName(sg) = %q, want SG", got)
	}
	if got := RegionName("de"); got != "DE" {
		t.Errorf("RegionName(de) = %q, want DE", got)
	}
}

func TestRegionEmojiFromAnyCountryCode(t *testing.T) {
	// "other" keeps the globe.
	if got := RegionEmoji(RegionOther); got != "🌍" {
		t.Errorf("RegionEmoji(other) = %q, want 🌍", got)
	}
	// Any 2-letter ISO code yields the correct flag via regional indicators.
	cases := map[string]string{
		"jp": "🇯🇵", "us": "🇺🇸", "sg": "🇸🇬", "de": "🇩🇪", "gb": "🇬🇧",
	}
	for code, want := range cases {
		if got := RegionEmoji(code); got != want {
			t.Errorf("RegionEmoji(%q) = %q, want %q", code, got, want)
		}
	}
	// Garbage / wrong-length inputs return the question flag.
	if got := RegionEmoji(""); got != "❓" {
		t.Errorf("RegionEmoji(\"\") = %q, want ❓", got)
	}
	if got := RegionEmoji("usa"); got != "❓" {
		t.Errorf("RegionEmoji(usa) = %q, want ❓", got)
	}
}
