package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const chatHTTP1Cooldown = 5 * time.Minute

// A reset after HTTP 200 bypasses Client.Do's error path. Remember it per
// origin so the next chat does not keep using the failing HTTP/2 path. Never
// replay a partly delivered answer. Billing and other origins keep their pools.
type chatProtocolFallback struct {
	mu    sync.Mutex
	until map[string]time.Time
	base  *http.Client
	h1    *http.Client
}

func chatOrigin(u *url.URL) string { return u.Scheme + "://" + u.Host }

func (c *Client) chatHTTPFor(u *url.URL) *http.Client {
	base := c.chatHTTP()
	f := &c.chatFallback
	f.mu.Lock()
	defer f.mu.Unlock()
	key := chatOrigin(u)
	if !time.Now().Before(f.until[key]) {
		delete(f.until, key)
		return base
	}
	tr, ok := base.Transport.(*http.Transport)
	if !ok {
		return base // Preserve injected/custom transports.
	}
	if f.h1 == nil || f.base != base {
		clone := tr.Clone()
		clone.ForceAttemptHTTP2 = false
		clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		if clone.TLSClientConfig != nil {
			clone.TLSClientConfig = clone.TLSClientConfig.Clone()
			clone.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		h1 := *base
		h1.Transport = clone
		f.base, f.h1 = base, &h1
	}
	return f.h1
}

func (c *Client) noteChatHTTP2Failure(u *url.URL) {
	f := &c.chatFallback
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.until == nil {
		f.until = make(map[string]time.Time)
	}
	key := chatOrigin(u)
	wasActive := time.Now().Before(f.until[key])
	f.until[key] = time.Now().Add(chatHTTP1Cooldown)
	if !wasActive {
		log.Printf("WARN: [upstream] HTTP/2 chat interrupted host=%s; next chats use HTTP/1.1 for %s (current request is not replayed)", u.Host, chatHTTP1Cooldown)
	}
}

type chatProtocolBody struct {
	io.ReadCloser
	ctx    context.Context
	onFail func()
	once   sync.Once
}

func (b *chatProtocolBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.ctx.Err() == nil && isChatHTTP2Failure(err) {
		b.once.Do(b.onFail)
	}
	return n, err
}

func isChatHTTP2Failure(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// net/http's bundled HTTP/2 error type is not exported. Match its specific
	// stream-reset signature, not arbitrary cancellations or timeout strings.
	s := err.Error()
	return strings.Contains(s, "stream error: stream ID ") &&
		(strings.Contains(s, "INTERNAL_ERROR") || strings.Contains(s, "PROTOCOL_ERROR"))
}

func (c *Client) observeChatProtocol(body io.ReadCloser, ctx context.Context, u *url.URL, major int) io.ReadCloser {
	if major != 2 {
		return body
	}
	return &chatProtocolBody{ReadCloser: body, ctx: ctx, onFail: func() { c.noteChatHTTP2Failure(u) }}
}
