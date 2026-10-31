package api

// BrowserLogoutBody contains only a same-origin continuation path. Provider
// token hints and state stay in encrypted HttpOnly cookies, never this response.
type BrowserLogoutBody struct {
	ContinuePath string `json:"continuePath,omitempty"`
}
