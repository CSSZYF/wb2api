package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Exercise real TLS/ALPN and an HTTP/2 RST_STREAM from net/http, rather than
// only matching a synthetic error string or inspecting transport flags.
func TestChatHTTP2ResetFallsBackForNextRequest(t *testing.T) {
	var mu sync.Mutex
	var protocols []int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protocols = append(protocols, r.ProtoMajor)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		if r.ProtoMajor == 2 {
			panic(http.ErrAbortHandler)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	c := New()
	c.ChatHTTP = srv.Client()
	c.ChatBaseCN = srv.URL
	defer func() {
		c.ChatHTTP.CloseIdleConnections()
		if c.chatFallback.h1 != nil {
			c.chatFallback.h1.CloseIdleConnections()
		}
	}()
	a := &auth.Auth{}
	for i := 0; i < 2; i++ {
		rc, status, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`), "", ChatMeta{})
		if err != nil || status != 200 {
			t.Fatalf("request %d: status=%d err=%v", i, status, err)
		}
		w := httptest.NewRecorder()
		err = Stream(w, rc)
		rc.Close()
		if i == 0 {
			if !IsStreamAbortedError(err) || !strings.Contains(w.Body.String(), "upstream_aborted") {
				t.Fatalf("partial first response hidden: err=%v body=%s", err, w.Body.String())
			}
		} else if err != nil || strings.Contains(w.Body.String(), "upstream_aborted") {
			t.Fatalf("fallback failed: err=%v body=%s", err, w.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(protocols) != 2 || protocols[0] != 2 || protocols[1] != 1 {
		t.Fatalf("expected one failing h2 and one h1, no replay: %v", protocols)
	}
	u, _ := url.Parse(srv.URL)
	other, _ := url.Parse("https://other.example")
	if c.chatHTTPFor(other) != c.ChatHTTP {
		t.Fatal("fallback leaked to another origin")
	}
	c.chatFallback.mu.Lock()
	c.chatFallback.until[chatOrigin(u)] = time.Now().Add(-time.Second)
	c.chatFallback.mu.Unlock()
	if c.chatHTTPFor(u) != c.ChatHTTP {
		t.Fatal("HTTP/2 was not restored after cooldown")
	}
}

type protocolErrorReader struct{ err error }

func (r protocolErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (r protocolErrorReader) Close() error             { return nil }

func TestChatProtocolFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		major    int
		err      error
		cancel   bool
		fallback bool
	}{
		{"h2-reset", 2, errors.New("stream error: stream ID 21; INTERNAL_ERROR; received from peer"), false, true},
		{"h2-truncated", 2, io.ErrUnexpectedEOF, false, true},
		{"h1-truncated", 1, io.ErrUnexpectedEOF, false, false},
		{"normal-eof", 2, io.EOF, false, false},
		{"client-cancel", 2, io.ErrUnexpectedEOF, true, false},
		{"idle-timeout", 2, context.Canceled, false, false},
		{"deadline", 2, context.DeadlineExceeded, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			u, _ := url.Parse("https://example.test/v2/chat/completions")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			b := c.observeChatProtocol(protocolErrorReader{tc.err}, ctx, u, tc.major)
			_, got := b.Read(make([]byte, 1))
			b.Close()
			if !errors.Is(got, tc.err) {
				t.Fatalf("read error changed: %v", got)
			}
			if active := len(c.chatFallback.until) != 0; active != tc.fallback {
				t.Fatalf("fallback=%v want %v", active, tc.fallback)
			}
		})
	}
}

func TestChatProtocolConcurrentFallback(t *testing.T) {
	c := New()
	u, _ := url.Parse("https://example.test/v2/chat/completions")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.noteChatHTTP2Failure(u)
			c.chatHTTPFor(u)
		}()
	}
	wg.Wait()
	if c.chatFallback.h1 == nil {
		t.Fatal("fallback transport was not created")
	}
	if c.HTTP.Transport == c.chatFallback.h1.Transport {
		t.Fatal("chat fallback replaced the billing transport")
	}
	c.chatFallback.h1.CloseIdleConnections()
}
