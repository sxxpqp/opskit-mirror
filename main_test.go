package main

import (
	"io"
	"log/slog"
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

func TestConfigDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("MIRROR_LISTEN", ":9090")
	t.Setenv("MIRROR_DOCKERHUB_USERNAME", "test-user")
	t.Setenv("MIRROR_DOCKERHUB_PASSWORD", "test-token")
	t.Setenv("MIRROR_DOCKERHUB_REDIRECT_HOSTS", "one.example, two.example")
	c, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	r := c.Registries["dockerhub"]
	if c.Listen != ":9090" || r.Username != "test-user" || r.PasswordEnv != "MIRROR_DOCKERHUB_PASSWORD" || len(r.RedirectHosts) != 2 {
		t.Fatalf("overrides failed")
	}
	if _, err := loadConfig("missing-config.yaml"); err == nil {
		t.Fatal("explicit missing file should fail")
	}
}

// Exercise the registry.k8s.io redirect observed in real containerd pulls.
func TestDefaultK8sRedirect(t *testing.T) {
	c, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != method {
					t.Errorf("method = %s, want %s", r.Method, method)
				}
				switch calls {
				case 1:
					if r.URL.Host != "registry.k8s.io" || r.URL.Path != "/v2/pause/manifests/3.9" {
						t.Fatalf("unexpected upstream: %s", r.URL)
					}
					return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{
						"Location": {"https://us-west2-docker.pkg.dev/v2/k8s-artifacts-prod/images/pause/manifests/3.9"},
					}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				case 2:
					if r.URL.Host != "us-west2-docker.pkg.dev" || r.URL.Path != "/v2/k8s-artifacts-prod/images/pause/manifests/3.9" {
						t.Fatalf("unexpected redirect: %s", r.URL)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				default:
					t.Fatalf("unexpected request: %s", r.URL)
					return nil, nil
				}
			})}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, httptest.NewRequest(method, "https://hub.sxxpqp.top/v2/k8s/pause/manifests/3.9?ns=registry.k8s.io", nil))
			if w.Code != http.StatusOK || calls != 2 {
				t.Fatalf("status = %d, upstream calls = %d; want 200 and 2", w.Code, calls)
			}
		})
	}
}

func TestDeniedRedirectLogsHostWithoutSecrets(t *testing.T) {
	for _, path := range []string{
		"/v2/k8s/pause/manifests/3.9",
		"/v2/k8s/pause/blobs/sha256:test",
		"/v2/quay/prometheus/busybox/blobs/sha256:test",
	} {
		t.Run(path, func(t *testing.T) {
			var logs strings.Builder
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			c, err := loadConfig("")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{
					"Location": {"https://user:secret-password@unlisted.example/private-path?token=secret-token"},
				}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "https://hub.sxxpqp.top"+path, nil)
			r.Header.Set("X-Request-ID", "redirect-test")
			m.ServeHTTP(w, r)
			if w.Code != http.StatusBadGateway || calls != 1 {
				t.Fatalf("status = %d, calls = %d; redirect must remain blocked", w.Code, calls)
			}
			for _, want := range []string{`"msg":"redirect_denied"`, `"redirect_host":"unlisted.example"`, `"request_id":"redirect-test"`} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("missing diagnostic %s: %s", want, logs.String())
				}
			}
			for _, secret := range []string{"secret-password", "secret-token", "private-path"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("logs leaked %s", secret)
				}
			}
		})
	}
}
