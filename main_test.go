package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPathAuth(t *testing.T) {
	m := &mirror{Config{Registries: map[string]Registry{"dockerhub": {Upstream: "https://registry-1.docker.io", AuthHosts: []string{"auth.docker.io"}}}}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/library/nginx/manifests/test" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Host != "registry-1.docker.io" {
			t.Errorf("host %s", r.Host)
		}
		if r.URL.Query().Get("ns") != "" {
			t.Error("ns leaked")
		}
		return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": {`Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`}}, Body: io.NopCloser(strings.NewReader("challenge")), Request: r}, nil
	})}
	r := httptest.NewRequest("GET", "https://sxxpqp.top/v2/dockerhub/library/nginx/manifests/test?ns=docker.io", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "https://sxxpqp.top/auth/dockerhub/") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}
func TestDenyAuthAndWrites(t *testing.T) {
	m := &mirror{config: Config{Registries: map[string]Registry{"dockerhub": {Upstream: "https://registry-1.docker.io"}}}}
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"POST", "/v2/", 405}, {"GET", "/auth/dockerhub/aHR0cHM6Ly9ldmlsLmV4YW1wbGUvdG9rZW4", 400}, {"GET", "/healthz", 200}} {
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code {
			t.Errorf("%s: %d", tc.path, w.Code)
		}
	}
}
