package blockpage

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

// RequestPath receives the unblock request form (POST). A dot-prefixed
// segment no site a user means to open is likely to have; every other
// path, and GET of this one, still renders the page.
const RequestPath = "/.kudurru/request"

// flashParam carries the outcome of a POST to the page it redirects to.
const flashParam = "kudurru-request"

// Form fields and their limits (the UnblockRequest schema bounds).
const (
	fieldName      = "name"
	fieldContact   = "contact"
	fieldReason    = "reason"
	fieldHoneypot  = "website" // hidden from people; bots fill it in
	maxNameLen     = 100
	maxContactLen  = 254
	maxReasonLen   = 500
	maxRequestBody = 8 << 10
)

// Outcomes of a POST, the flash values the page knows.
const (
	flashSent    = "sent"
	flashLimited = "limited"
	flashPaused  = "paused"
	flashInvalid = "invalid"
	flashError   = "error"
)

// nodeWrites bounds the requests one node writes to the store per minute,
// whatever the client addresses (bursts of the same size).
const nodeWrites = 30

// RequestBook stores the unblock requests; *unblock.Service implements it.
type RequestBook interface {
	Lookup(client netip.Addr, domain string) *v1alpha1.UnblockRequest
	Submit(ctx context.Context, sub unblock.Submission) (*v1alpha1.UnblockRequest, error)
	Counts() unblock.Counts
	Settings() config.UnblockRequests
}

// RequestView is the unblock request section of the page.
type RequestView struct {
	// Action is the form's target (RequestPath); Disabled renders the
	// form inert (the admin preview).
	Action   string
	Disabled bool
	// Flash is the message of the POST that redirected here.
	Flash string
	// Status describes the client's request for the name; Note is the
	// administrator's note.
	Status string
	Note   string
	// Form shows the form, Update labels it as a change of a pending one.
	Form   bool
	Update bool
}

// flashText maps the flash parameter to its message; unknown values show
// nothing.
func flashText(v string) string {
	switch v {
	case flashSent:
		return "Your request was sent to the administrator."
	case flashLimited:
		return "Too many requests from your device. Try again later."
	case flashPaused:
		return "Requests are paused because too many are waiting for a decision. Try again later."
	case flashInvalid:
		return "The request could not be read. Check the fields and try again."
	case flashError:
		return "The request could not be saved. Try again later."
	}
	return ""
}

// requestView builds the section for client and domain, nil when
// requests are off or the page names no domain.
func (s *Server) requestView(st *Settings, client netip.Addr, domain, flash string, now time.Time, preview bool) *RequestView {
	if !st.Requests || domain == "" || (s.opts.Requests == nil && !preview) {
		return nil
	}
	v := &RequestView{Action: RequestPath, Disabled: preview, Flash: flashText(flash), Form: true}
	if preview {
		return v
	}
	book := s.opts.Requests
	r := book.Lookup(client, domain)
	if r != nil {
		v.Note = r.Status.Note
		switch r.EffectiveState() {
		case v1alpha1.UnblockPending:
			v.Status = "You asked for access on " + timeText(unblock.RequestedAt(r)) + ". An administrator has not decided yet; you can add details below."
			v.Update = true
			return v
		case v1alpha1.UnblockApproved:
			if unblock.Active(r, now) {
				v.Form = false
				v.Status = "An administrator allowed this name" + approvalUntil(r) + ". Try again in a moment" + retryHint(r, st.BlockTTL, now) + "."
				return v
			}
			v.Status = "Your earlier access to this name has ended. You can ask again."
		case v1alpha1.UnblockDenied:
			v.Status = "An administrator declined your request" + decidedOn(r) + ". You can ask again."
		case v1alpha1.UnblockExpired:
			v.Status = "Your earlier request has ended. You can ask again."
		}
	}
	if book.Counts().Pending >= book.Settings().MaxPending {
		v.Form = false
		if v.Status == "" {
			v.Status = flashText(flashPaused)
		}
	}
	return v
}

func timeText(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func decidedOn(r *v1alpha1.UnblockRequest) string {
	if r.Status.DecidedAt == nil {
		return ""
	}
	return " on " + timeText(r.Status.DecidedAt.Time)
}

func approvalUntil(r *v1alpha1.UnblockRequest) string {
	if a := r.Status.Allow; a != nil && a.ExpiresAt != nil {
		return " until " + timeText(a.ExpiresAt.Time)
	}
	return ""
}

// retryHint says how long the device may still hold the block answer: a
// block answer cached before the decision lives at most the block TTL
// after it.
func retryHint(r *v1alpha1.UnblockRequest, ttl time.Duration, now time.Time) string {
	if ttl <= 0 || r.Status.DecidedAt == nil {
		return ""
	}
	left := r.Status.DecidedAt.Add(ttl).Sub(now)
	if left <= 0 {
		return "; if this page still appears, ask your administrator"
	}
	mins := int((left + time.Minute - 1) / time.Minute)
	unit := "minutes"
	if mins == 1 {
		unit = "minute"
	}
	return ": your device may remember the blocked answer for up to " + strconv.Itoa(mins) + " more " + unit
}

// formText cleans one form field: CRLF to LF, control and bidi-override
// characters dropped (line breaks kept where multiline), trimmed. ok is
// false for invalid UTF-8 or more than limit characters.
func formText(v string, limit int, multiline bool) (string, bool) {
	if !utf8.ValidString(v) {
		return "", false
	}
	v = strings.ReplaceAll(v, "\r\n", "\n")
	var b strings.Builder
	for _, c := range v {
		switch {
		case c == '\n' && multiline:
			b.WriteRune(c)
		case c == '\n' || c == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(c), c >= 0x202A && c <= 0x202E, c >= 0x2066 && c <= 0x2069:
		default:
			b.WriteRune(c)
		}
	}
	out := strings.TrimSpace(b.String())
	return out, utf8.RuneCountInString(out) <= limit
}

// sameOrigin rejects cross-site form posts: the block page sets no
// cookies and asks for no credentials, so a forged post can only file a
// request in the victim's name, and Origin (sent with a POST under the
// page's same-origin referrer policy) or Sec-Fetch-Site tells it apart.
// Clients that send neither (curl, old browsers) pass.
func sameOrigin(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// handleRequest takes a POST of the form and redirects to the page with
// the outcome (POST-redirect-GET).
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	st := s.settings.Load()
	if s.opts.Requests == nil || !st.Requests {
		http.Error(w, "unblock requests are off on this server", http.StatusNotFound)
		return
	}
	domain := requestDomain(r)
	if domain == "" {
		http.Error(w, "the request names no domain", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		s.unblockResult("cross_origin")
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/x-www-form-urlencoded") {
		http.Error(w, "send the form (application/x-www-form-urlencoded)", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := r.ParseForm(); err != nil {
		s.unblockResult(flashInvalid)
		http.Error(w, "the form is too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}
	redirect := func(flash string) {
		s.unblockResult(flash)
		w.Header().Set("Location", "/?"+url.Values{flashParam: {flash}}.Encode())
		w.WriteHeader(http.StatusSeeOther)
	}
	if r.PostForm.Get(fieldHoneypot) != "" {
		redirect(flashSent) // look accepted, store nothing
		return
	}
	name, ok1 := formText(r.PostForm.Get(fieldName), maxNameLen, false)
	contact, ok2 := formText(r.PostForm.Get(fieldContact), maxContactLen, false)
	reason, ok3 := formText(r.PostForm.Get(fieldReason), maxReasonLen, true)
	if !ok1 || !ok2 || !ok3 {
		redirect(flashInvalid)
		return
	}
	client := remoteAddr(r)
	if !s.reqLimiter.allow(client) || !s.writes.Allow() {
		redirect(flashLimited)
		return
	}
	now := s.opts.Now()
	sub := unblock.Submission{Client: client, Domain: domain, Name: name, Contact: contact, Reason: reason}
	if s.opts.Membership != nil && client.IsValid() {
		sub.Groups, sub.Policies = s.opts.Membership(client, now)
	}
	if s.opts.Finder != nil {
		fctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if rs, ok, _ := s.opts.Finder.Find(fctx, client, domain, now); ok {
			if rs.Policy == "" && s.opts.Policy != nil && client.IsValid() {
				rs.Policy = s.opts.Policy(client, now)
			}
			sub.Block = &unblock.Block{Kind: rs.Kind, List: rs.List, Rule: rs.Rule, Policy: rs.Policy}
		}
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	_, err := s.opts.Requests.Submit(ctx, sub)
	switch {
	case errors.Is(err, unblock.ErrPaused):
		redirect(flashPaused)
	case err != nil:
		s.opts.Logger.Warn("unblock request not stored", "domain", domain, "err", err)
		redirect(flashError)
	default:
		s.opts.Logger.Info("unblock request received", "domain", domain)
		redirect(flashSent)
	}
}

func (s *Server) unblockResult(result string) {
	s.unblockRequests.WithLabelValues(result).Inc()
}
