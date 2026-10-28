package util

import (
	"net/url"
	"strings"
)

// OpenDesignAuthBaseURL is the dashboard API host; tests may override it.
var OpenDesignAuthBaseURL = "https://amr-api.open-design.ai"

// IsOpenDesignBaseURL reports whether the URL belongs to OpenDesign.
func IsOpenDesignBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.Contains(strings.ToLower(u.Hostname()), "open-design.ai")
}

// OpenDesignAuthURL constructs a dashboard API URL.
func OpenDesignAuthURL(path string) string {
	return strings.TrimRight(OpenDesignAuthBaseURL, "/") + path
}
