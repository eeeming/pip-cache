package proxy

import (
	"fmt"
	"net/http"
	"strings"
)

// Upstream represents an upstream PyPI server
type Upstream struct {
	Name    string
	BaseURL string
	// PathPrefix is added to the request path when proxying (e.g., "/pypi" for aliyun)
	PathPrefix string
	// Client is the HTTP client used for requests
	Client *http.Client
}

// BuildURL constructs the full URL for a request
func (u *Upstream) BuildURL(path string) string {
	// Remove leading slash from path
	path = strings.TrimPrefix(path, "/")

	// Add path prefix if configured
	if u.PathPrefix != "" {
		path = strings.TrimPrefix(u.PathPrefix, "/") + "/" + path
	}

	return fmt.Sprintf("%s/%s", strings.TrimSuffix(u.BaseURL, "/"), path)
}

// NewUpstream creates a new upstream configuration
func NewUpstream(name, baseURL, pathPrefix string, client *http.Client) *Upstream {
	return &Upstream{
		Name:       name,
		BaseURL:    baseURL,
		PathPrefix: pathPrefix,
		Client:     client,
	}
}

// IsRetriableError checks if an error should trigger a fallback
func IsRetriableError(statusCode int) bool {
	// Retry on server errors and specific client errors
	return statusCode >= 500 ||
		statusCode == http.StatusNotFound ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
}
