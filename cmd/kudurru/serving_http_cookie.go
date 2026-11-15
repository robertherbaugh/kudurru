package main

import (
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
)

func (h *servingHTTP) cookiePolicy(r *http.Request, suffix string) (session.CookieConfig, error) {
	c, err := servinghttp.CookiePolicy(r.Context(), h.opts.Cookie)
	if err != nil {
		return session.CookieConfig{}, err
	}
	c.Name += suffix
	return c, nil
}

func (h *servingHTTP) setCookie(w http.ResponseWriter, r *http.Request, suffix, value string, life time.Duration) error {
	c, err := h.cookiePolicy(r, suffix)
	if err != nil {
		return err
	}
	c.Set(w, r, value, life)
	return nil
}

func (h *servingHTTP) clearCookie(w http.ResponseWriter, r *http.Request, suffix string) error {
	c, err := h.cookiePolicy(r, suffix)
	if err != nil {
		return err
	}
	c.Clear(w, r)
	return nil
}

// checkCookiePublication checks only a sealing response's policy compatibility.
// It does not replace the ORIGINAL request View, reacquire authority or retry an
// enrollment. A changed policy requires a fresh sign-in request.
func (h *servingHTTP) checkCookiePublication(r *http.Request, v *servingauth.View) error {
	original, err := h.cookiePolicy(r, "")
	if err != nil {
		return err
	}
	next, err := servinghttp.CookiePolicyForView(v, h.opts.Cookie)
	if err != nil {
		return err
	}
	if original != next {
		return serving.ErrSuperseded
	}
	return nil
}
