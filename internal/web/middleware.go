package web

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const (
	csrfHeader = "X-CSRF-Token"
	csrfField  = "_csrf"
)

func (h *Handler) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				h.logger.Error("handler panicked", zap.String("path", r.URL.Path), zap.Any("panic", v), zap.Stack("stack"))
				if isAPI(r) {
					h.apiWrite(w, answerEncoding(r), http.StatusInternalServerError, &pb.ApiError{Code: apiCode(ierrors.CodeInternal), Message: "internal error"})
					return
				}
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", h.csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		// Voice channels take the microphone, the camera and the screen.
		hd.Set("Permissions-Policy", "camera=(self), microphone=(self), display-capture=(self), geolocation=(), payment=(), usb=(), interest-cohort=()")
		if h.secure {
			hd.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		// no-transform keeps proxies such as Cloudflare from rewriting pages
		// and injecting their own scripts.
		hd.Set("Cache-Control", "no-store, no-transform")
		hd.Set("Vary", "HX-Request, HX-Target")
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) throttle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := h.allow(r); !ok {
			retryAfter(w, wait)
			if isAPI(r) {
				h.apiWrite(w, answerEncoding(r), http.StatusTooManyRequests, &pb.ApiError{
					Code: apiCode(ierrors.CodeResourceExhausted), Message: "too many requests, slow down",
				})
				return
			}
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiSharedFactor is how many sessions' worth of signed-in API calls one
// address may make: phones behind a carrier's address share it.
const apiSharedFactor = 10

// allow takes a request from its budget. An app's signed-in calls count
// against its session, so the phones sharing a carrier's address do not
// share one budget, and against a larger one of the address, so tokens made
// up to get fresh budgets buy little.
func (h *Handler) allow(r *http.Request) (bool, time.Duration) {
	ip := h.clientIP(r)
	if token := apiToken(r); token != "" && isAPI(r) {
		if ok, wait := h.apiShared.Allow(ip); !ok {
			return false, wait
		}
		return h.apiSessions.Allow("session:" + secure.PublicID(secure.Hash(token)))
	}
	return h.reqs.Allow(ip)
}

// isAPI reports whether r is a call of the client API or of the GoChat
// protocol, which answer ApiErrors.
func isAPI(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/v1/") || strings.HasPrefix(r.URL.Path, "/api/v2/")
}

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
}

// limited applies the stricter budget of the authentication endpoints.
func (h *Handler) limited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := h.auth.Allow(h.clientIP(r)); !ok {
			retryAfter(w, wait)
			w.Header().Set("X-Error", "Too many attempts, wait a minute.")
			http.Error(w, "too many attempts, wait a minute", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

const maxFormBytes = 1 << 20

// csrf requires a same-origin request carrying the token of the CSRF cookie,
// in the X-CSRF-Token header (htmx) or the _csrf field (plain forms).
func (h *Handler) csrf(next http.Handler) http.Handler { return h.guard(next, false) }

// csrfMultipart guards a handler that reads a multipart body and bounds it
// itself.
func (h *Handler) csrfMultipart(next http.Handler) http.Handler { return h.guard(next, true) }

func (h *Handler) guard(next http.Handler, multipart bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.sameOrigin(r) {
			h.logger.Warn("cross-origin request refused", zap.String("path", r.URL.Path), zap.String("origin", r.Header.Get("Origin")))
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if !multipart {
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				http.Error(w, "unsupported request body", http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		c, err := r.Cookie(h.cookieName("csrf"))
		if err != nil || !secure.ValidToken(c.Value) {
			h.refuseCSRF(w, r)
			return
		}
		sent := r.Header.Get(csrfHeader)
		if sent == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			sent = r.PostFormValue(csrfField)
		}
		// Pages carry the token masked.
		if token, ok := secure.UnmaskToken(sent); ok {
			sent = token
		}
		if !secure.Equal(sent, c.Value) {
			h.refuseCSRF(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) refuseCSRF(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("X-Error", "The page expired, reload it.")
		http.Error(w, "the page expired, reload it", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, safeNext(r.URL.Path), http.StatusSeeOther) //nolint:gosec // safeNext keeps it on this site
}

func (h *Handler) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == h.origin
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// cookieName gives a cookie the __Host- prefix when cookies are Secure, which
// binds it to this host and path /.
func (h *Handler) cookieName(name string) string {
	if h.secure {
		return "__Host-gochat_" + name
	}
	return "gochat_" + name
}

func (h *Handler) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration, sameSite http.SameSite) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the public URL's scheme
		Name:     h.cookieName(name),
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   h.secure,
		SameSite: sameSite,
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the public URL's scheme
		Name: h.cookieName(name), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
}

// csrfToken returns the request's CSRF token for a page, masked, issuing
// one when missing.
func (h *Handler) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(h.cookieName("csrf")); err == nil && secure.ValidToken(c.Value) {
		return secure.MaskToken(c.Value)
	}
	// Lax: a visit from another site does not see a Strict cookie, and the
	// new token it got would break every other open tab.
	token := secure.Token()
	h.setCookie(w, "csrf", token, 0, http.SameSiteLaxMode)
	return secure.MaskToken(token)
}

func (h *Handler) sessionToken(r *http.Request) string {
	c, err := r.Cookie(h.cookieName("session"))
	if err != nil || !secure.ValidToken(c.Value) {
		return ""
	}
	return c.Value
}
