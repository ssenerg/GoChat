// Package unfurl reads what a linked page says about itself. The links come
// from users, so it fetches as a server must that takes URLs on anyone's
// word: public addresses only, checked at every hop and pinned for the
// connection, within limits of size, time and redirects, with no cookies
// or credentials.
package unfurl

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/idna"

	"github.com/ssenerg/gochat/internal/core/netguard"
)

var (
	ErrURL       = errors.New("not an http or https URL on an allowed port")
	ErrHost      = errors.New("the host is not allowed")
	ErrAddress   = errors.New("the host resolves to an address that is not public")
	ErrRedirects = errors.New("too many redirects")
	ErrStatus    = errors.New("the server did not answer with the page")
	ErrType      = errors.New("not a kind of content taken here")
	ErrTooLarge  = errors.New("the response is too large")
)

// Resolver looks host names up.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Fetcher fetches pages and pictures for previews.
type Fetcher struct {
	// Timeout bounds one fetch, redirects and reading the body included.
	Timeout      time.Duration
	MaxRedirects int
	// Ports are those a URL may name; 80 and 443 when empty.
	Ports     []int
	UserAgent string
	// Permit says whether an address may be reached; nil takes the public
	// ones only.
	Permit func(netip.Addr) bool
	// Resolver looks names up; nil uses the system's.
	Resolver Resolver
}

// Request is one fetch.
type Request struct {
	URL string
	// Accept is sent as the Accept header; Takes says whether to read a
	// response of a media type.
	Accept string
	Takes  func(mediaType string) bool
	// MaxBytes bounds the body. With Truncate, a longer body is cut there
	// instead of refused: a page's head comes first.
	MaxBytes int64
	Truncate bool
	// Host says whether a host may be fetched from, at every hop; nil
	// takes any.
	Host func(host string) bool
}

// Response is a fetched body.
type Response struct {
	// URL is where the fetch ended, after redirects.
	URL *url.URL
	// MediaType is the response's, without parameters; ContentType has them.
	MediaType   string
	ContentType string
	Body        []byte
	Truncated   bool
}

// Fetch gets req.URL, following redirects within the limits.
func (f *Fetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	u, err := f.check(req.URL, req.Host)
	if err != nil {
		return nil, err
	}
	for hops := 0; ; hops++ {
		res, err := f.hop(ctx, u, req)
		if err != nil {
			return nil, err
		}
		if !redirect(res.StatusCode) {
			defer func() { _ = res.Body.Close() }()
			return f.read(u, res, req)
		}
		loc := res.Header.Get("Location")
		_ = res.Body.Close()
		if hops >= f.MaxRedirects {
			return nil, ErrRedirects
		}
		next, err := u.Parse(loc)
		if err != nil || loc == "" {
			return nil, ErrStatus
		}
		if u, err = f.check(next.String(), req.Host); err != nil {
			return nil, err
		}
	}
}

func redirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// check parses raw and refuses what may not be fetched: another scheme,
// credentials, a port not allowed, a host the policy refuses. The host
// comes back in its ASCII form.
func (f *Fetcher) check(raw string, allowed func(string) bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" || len(raw) > MaxURL {
		return nil, ErrURL
	}
	name := strings.TrimSuffix(u.Hostname(), ".")
	var host string
	if addr, err := netip.ParseAddr(name); err == nil {
		if addr.Zone() != "" {
			return nil, ErrURL
		}
		host = addr.String()
	} else if host, err = idna.Lookup.ToASCII(name); err != nil || host == "" || strings.ContainsAny(host, ":%") {
		return nil, ErrURL
	}
	port, ok := portOf(u)
	if !ok || !slices.Contains(f.ports(), port) {
		return nil, ErrURL
	}
	if allowed != nil && !allowed(host) {
		return nil, ErrHost
	}
	if explicit := u.Port(); explicit != "" {
		u.Host = net.JoinHostPort(host, explicit)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// portOf is the port u names, or its scheme's.
func portOf(u *url.URL) (int, bool) {
	if u.Port() == "" {
		return map[string]int{"http": 80, "https": 443}[u.Scheme], true
	}
	n, err := strconv.Atoi(u.Port())
	return n, err == nil && n > 0 && n < 65536
}

func (f *Fetcher) ports() []int {
	if len(f.Ports) == 0 {
		return []int{80, 443}
	}
	return f.Ports
}

func (f *Fetcher) permit(addr netip.Addr) bool {
	if f.Permit != nil {
		return f.Permit(addr.Unmap())
	}
	return netguard.Public(addr)
}

// resolve returns the addresses of host, refusing them all when any is
// not permitted: a name that points inward as well is not followed.
func (f *Fetcher) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if !f.permit(addr) {
			return nil, ErrAddress
		}
		return []netip.Addr{addr.Unmap()}, nil
	}
	r := f.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no address", host)
	}
	for i, a := range addrs {
		if !f.permit(a) {
			return nil, ErrAddress
		}
		addrs[i] = a.Unmap()
	}
	return addrs, nil
}

// hop makes one request, its connection pinned to the addresses checked
// for its host, so a second lookup cannot swap them.
func (f *Fetcher) hop(ctx context.Context, u *url.URL, req Request) (*http.Response, error) {
	addrs, err := f.resolve(ctx, u.Hostname())
	if err != nil {
		return nil, err
	}
	port, _ := portOf(u)
	dialer := &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		// Again at the socket, should anything dial past the pin.
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !f.permit(ap.Addr()) {
			return ErrAddress
		}
		return nil
	}}
	pinned := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var last error
		for _, a := range addrs[:min(len(addrs), 4)] {
			conn, err := dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String()) //nolint:gosec // a checked port
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            pinned,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:      true,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrURL
	}
	hr.Header.Set("User-Agent", f.UserAgent)
	if req.Accept != "" {
		hr.Header.Set("Accept", req.Accept)
	}
	hr.Header.Set("Accept-Language", "en;q=1, *;q=0.5")
	return client.Do(hr)
}

// read takes the body of a final response within the request's limits.
func (f *Fetcher) read(u *url.URL, res *http.Response, req Request) (*Response, error) {
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("%w: %d", ErrStatus, res.StatusCode)
	}
	ctype := res.Header.Get("Content-Type")
	media, _, _ := mime.ParseMediaType(ctype)
	if req.Takes != nil && !req.Takes(media) {
		return nil, ErrType
	}
	if res.ContentLength > req.MaxBytes && !req.Truncate {
		return nil, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, req.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	out := &Response{URL: u, MediaType: media, ContentType: ctype, Body: body}
	if int64(len(body)) > req.MaxBytes {
		if !req.Truncate {
			return nil, ErrTooLarge
		}
		out.Body, out.Truncated = body[:req.MaxBytes], true
	}
	return out, nil
}
