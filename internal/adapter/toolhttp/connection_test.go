package toolhttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
)

func TestHTTPPolicy(t *testing.T) {
	for _, mode := range []string{"timeout", "disconnect", "large", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect followed") }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				switch mode {
				case "timeout":
					<-r.Context().Done()
				case "disconnect":
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
				case "large":
					_, _ = io.WriteString(w, strings.Repeat("x", 4096))
				case "redirect":
					http.Redirect(w, r, target.URL, 302)
				}
			}))
			defer server.Close()
			c, err := New(Config{Scope: domain.Scope{TenantID: "t", UserID: "u"}, ID: "c", URL: server.URL, Timeout: 50 * time.Millisecond, MaxBytes: 1024})
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(context.Background(), "POST", server.URL, strings.NewReader("{}"))
			resp, err := c.HTTPClient().Do(req)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if err == nil {
				t.Fatal("accepted failure")
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatal("replayed side effect")
			}
		})
	}
}
func TestCredentialsRotateAndRemainScoped(t *testing.T) {
	var lookups atomic.Int32
	scope := domain.Scope{TenantID: "t", UserID: "u"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+r.Header.Get("Expected") || r.Header.Get("Cookie") != "" {
			t.Error("wrong credentials")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	c, err := New(Config{Scope: scope, ID: "c", URL: server.URL, Timeout: time.Second, MaxBytes: 1024, CredentialRef: "primary", Credentials: func(_ context.Context, s domain.Scope, ref string) (string, error) {
		if s != scope {
			t.Error("wrong scope")
		}
		lookups.Add(1)
		return "Bearer " + ref, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"primary", "secondary", "primary"} {
		req, _ := http.NewRequestWithContext(WithCredentialRef(context.Background(), ref), "GET", server.URL, nil)
		req.Header.Set("Expected", ref)
		req.Header.Set("Cookie", "secret")
		resp, err := c.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	req, _ := http.NewRequest("GET", "http://different.invalid", nil)
	if _, err := c.HTTPClient().Do(req); err == nil {
		t.Fatal("cross-origin allowed")
	}
	if lookups.Load() != 3 {
		t.Fatal(lookups.Load())
	}
}
