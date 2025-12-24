package core

import (
	"testing"
	"time"
)

func TestGenerateKey(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{"GET", "/simple/", "GET-/simple/"},
		{"get", "/simple/package/", "GET-/simple/package/"},
		{"POST", "/packages/", "POST-/packages/"},
		{"GET", "/simple/package/?version=1.0", "GET-/simple/package/"},
	}

	for _, tt := range tests {
		got := GenerateKey(tt.method, tt.path)
		if got != tt.want {
			t.Errorf("GenerateKey(%q, %q) = %q, want %q", tt.method, tt.path, got, tt.want)
		}
	}
}

func TestGetTTLForPath(t *testing.T) {
	simpleTTL := 10 * time.Minute
	packagesTTL := 24 * time.Hour
	defaultTTL := 1 * time.Hour

	tests := []struct {
		path string
		want time.Duration
	}{
		{"/simple/", simpleTTL},
		{"/simple/package/", simpleTTL},
		{"/packages/", packagesTTL},
		{"/packages/package.whl", packagesTTL},
		{"/other/", defaultTTL},
		{"/", defaultTTL},
	}

	for _, tt := range tests {
		got := GetTTLForPath(tt.path, simpleTTL, packagesTTL, defaultTTL)
		if got != tt.want {
			t.Errorf("GetTTLForPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestEntryIsExpired(t *testing.T) {
	now := time.Now()
	
	tests := []struct {
		name     string
		expiresAt time.Time
		want     bool
	}{
		{"not expired", now.Add(1 * time.Hour), false},
		{"expired", now.Add(-1 * time.Hour), true},
		{"just expired", now.Add(-1 * time.Second), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &Entry{
				ExpiresAt: tt.expiresAt,
			}
			got := entry.IsExpired()
			if got != tt.want {
				t.Errorf("IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHashKeyWithDir(t *testing.T) {
	key := "GET-/simple/test/"
	dir, filename := HashKeyWithDir(key)

	if len(dir) != 3 {
		t.Errorf("HashKeyWithDir() dir length = %d, want 3", len(dir))
	}

	if len(filename) != 29 {
		t.Errorf("HashKeyWithDir() filename length = %d, want 29", len(filename))
	}

	// 测试相同key产生相同hash
	dir2, filename2 := HashKeyWithDir(key)
	if dir != dir2 || filename != filename2 {
		t.Errorf("HashKeyWithDir() should produce consistent results")
	}
}

