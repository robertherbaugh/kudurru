package api

// BrowserLogoutResponse contains only a same-origin continuation path. Provider
// token hints and state stay in encrypted HttpOnly cookies, never this response.
type BrowserLogoutResponse struct {
	ContinuePath string `json:"continuePath,omitempty"`
}
