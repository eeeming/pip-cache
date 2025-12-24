package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestGetContentLength(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    int64
		wantOk  bool
	}{
		{
			name: "valid content length",
			headers: http.Header{
				"Content-Length": []string{"1234"},
			},
			want:   1234,
			wantOk: true,
		},
		{
			name: "no content length",
			headers: http.Header{},
			want:   0,
			wantOk: false,
		},
		{
			name: "invalid content length",
			headers: http.Header{
				"Content-Length": []string{"invalid"},
			},
			want:   0,
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := GetContentLength(tt.headers)
			if got != tt.want || ok != tt.wantOk {
				t.Errorf("GetContentLength() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOk)
			}
		})
	}
}

func TestIsRetriableError(t *testing.T) {
	tests := []struct {
		statusCode int
		want       bool
	}{
		{http.StatusOK, false},
		{http.StatusNotFound, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
	}

	for _, tt := range tests {
		got := IsRetriableError(tt.statusCode)
		if got != tt.want {
			t.Errorf("IsRetriableError(%d) = %v, want %v", tt.statusCode, got, tt.want)
		}
	}
}

func TestIsLargeFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/packages/test.whl", true},
		{"/packages/test.tar.gz", true},
		{"/packages/test.zip", true},
		{"/simple/test/", false},
		{"/packages/test.txt", false},
	}

	for _, tt := range tests {
		got := IsLargeFile(tt.path)
		if got != tt.want {
			t.Errorf("IsLargeFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestUpstream_BuildURL(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	
	tests := []struct {
		name       string
		upstream   *Upstream
		path       string
		want       string
	}{
		{
			name: "no prefix",
			upstream: NewUpstream("test", "https://example.com", "", client),
			path:     "/simple/test/",
			want:     "https://example.com/simple/test/",
		},
		{
			name: "with prefix",
			upstream: NewUpstream("test", "https://example.com", "/pypi", client),
			path:     "/simple/test/",
			want:     "https://example.com/pypi/simple/test/",
		},
		{
			name: "path without leading slash",
			upstream: NewUpstream("test", "https://example.com", "", client),
			path:     "simple/test/",
			want:     "https://example.com/simple/test/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.upstream.BuildURL(tt.path)
			if got != tt.want {
				t.Errorf("BuildURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProxyClient_Fetch(t *testing.T) {
	// 创建测试服务器
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("test response"))
	}))
	defer server.Close()

	// 创建代理客户端
	client := &http.Client{Timeout: 5 * time.Second}
	primary := NewUpstream("test", server.URL, "", client)
	fallback := NewUpstream("fallback", server.URL, "", client)
	
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // 测试时只显示错误
	proxyClient := NewProxyClient(primary, fallback, logger)

	// 测试Fetch
	resp, err := proxyClient.Fetch("GET", "/test", make(http.Header))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Fetch() StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	if string(resp.Body) != "test response" {
		t.Errorf("Fetch() Body = %q, want %q", string(resp.Body), "test response")
	}
}

func TestProxyClient_GetStreamingResponse(t *testing.T) {
	// 创建测试服务器
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "13")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("test response"))
	}))
	defer server.Close()

	// 创建代理客户端
	client := &http.Client{Timeout: 5 * time.Second}
	primary := NewUpstream("test", server.URL, "", client)
	fallback := NewUpstream("fallback", server.URL, "", client)
	
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // 测试时只显示错误
	proxyClient := NewProxyClient(primary, fallback, logger)

	// 测试GetStreamingResponse
	body, headers, statusCode, upstream, err := proxyClient.GetStreamingResponse("GET", "/test", make(http.Header))
	if err != nil {
		t.Fatalf("GetStreamingResponse() error = %v", err)
	}
	defer body.Close()

	if statusCode != http.StatusOK {
		t.Errorf("GetStreamingResponse() StatusCode = %d, want %d", statusCode, http.StatusOK)
	}

	if upstream != "test" {
		t.Errorf("GetStreamingResponse() Upstream = %q, want %q", upstream, "test")
	}

	if headers.Get("Content-Type") != "text/html" {
		t.Errorf("GetStreamingResponse() Content-Type = %q, want %q", headers.Get("Content-Type"), "text/html")
	}

	// 读取body
	buf := make([]byte, 100)
	n, err := body.Read(buf)
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("Read() error = %v", err)
	}

		if string(buf[:n]) != "test response" {
		t.Errorf("GetStreamingResponse() body = %q, want %q", string(buf[:n]), "test response")
	}
}

