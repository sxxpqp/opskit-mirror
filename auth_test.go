package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestDockerPathAndContainerdTokenScopes(t *testing.T) {
	for _, tc := range []struct {
		name, namespace, upstreamRepo string
	}{
		{"docker", "", "stefanprodan/podinfo"},
		{"docker-prefixed-namespace", "", "ghcr/podinfo"},
		{"containerd", "ghcr.io", "stefanprodan/podinfo"},
		{"containerd-prefixed-namespace", "ghcr.io", "ghcr/podinfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadConfig("")
			if err != nil {
				t.Fatal(err)
			}
			m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/token" {
					if r.URL.Query().Get("repository_prefix") != "" || r.URL.Query().Get("repository_path") != "" {
						t.Fatal("routing metadata leaked upstream")
					}
					want := []string{"repository:" + tc.upstreamRepo + ":pull"}
					if !reflect.DeepEqual(r.URL.Query()["scope"], want) {
						t.Fatalf("token scopes = %v, want %v", r.URL.Query()["scope"], want)
					}
					if r.URL.Query().Get("service") != "ghcr.io" {
						t.Fatal("service changed")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"token":"test"}`)), Request: r}, nil
				}
				return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": {`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:` + tc.upstreamRepo + `:pull"`}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			path := "https://mirror.example/v2/ghcr/" + tc.upstreamRepo + "/manifests/test"
			if tc.namespace != "" {
				path += "?ns=" + tc.namespace
			}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 401 {
				t.Fatalf("challenge status %d", w.Code)
			}
			matches := realmRE.FindStringSubmatch(w.Header().Get("WWW-Authenticate"))
			if len(matches) != 2 {
				t.Fatal("missing rewritten realm")
			}
			u, err := url.Parse(matches[1])
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			q.Set("service", "ghcr.io")
			q.Add("scope", "repository:"+tc.upstreamRepo+":pull")
			if tc.namespace == "" {
				if q.Get("repository_prefix") != "ghcr" {
					t.Fatal("Docker routing context missing")
				}
				q.Add("scope", "repository:ghcr/"+tc.upstreamRepo+":pull")
			} else if q.Get("repository_prefix") != "" {
				t.Fatal("containerd scope must not be rewritten")
			}
			u.RawQuery = q.Encode()
			w = httptest.NewRecorder()
			m.ServeHTTP(w, httptest.NewRequest("GET", u.String(), nil))
			if w.Code != 200 {
				t.Fatalf("token status %d", w.Code)
			}
		})
	}
}

func TestRedirectPreservesMultipleAcceptHeaders(t *testing.T) {
	c, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	accepts := []string{"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.oci.image.index.v1+json"}
	calls := 0
	m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !reflect.DeepEqual(r.Header.Values("Accept"), accepts) {
			t.Fatalf("Accept truncated: %v", r.Header.Values("Accept"))
		}
		if calls == 1 {
			return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://cdn.example/manifest"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	r := httptest.NewRequest("GET", "https://mirror.example/v2/k8s/pause/manifests/3.9", nil)
	for _, accept := range accepts {
		r.Header.Add("Accept", accept)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != 200 || calls != 2 {
		t.Fatalf("status=%d calls=%d", w.Code, calls)
	}
}

func TestClassicDockerRootAuthentication(t *testing.T) {
	for _, registry := range []string{"ghcr", "quay"} {
		t.Run(registry, func(t *testing.T) {
			c, err := loadConfig("")
			if err != nil {
				t.Fatal(err)
			}
			c.Registries["dockerhub"] = Registry{Upstream: "https://registry-1.docker.io", AuthHosts: []string{"auth.docker.io"}, Username: "hub-user", PasswordEnv: "TEST_HUB_PASSWORD"}
			t.Setenv("TEST_HUB_PASSWORD", "hub-secret")
			upstream := registry + ".io"
			tokenHost := upstream
			service := upstream
			if registry == "quay" {
				service = "quay.io"
			}
			calls := 0
			m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path == "/v2/" {
					realm, audience := "https://auth.docker.io/token", "registry.docker.io"
					if r.URL.Host == upstream {
						if r.Header.Get("Authorization") != "" {
							t.Fatal("credentials leaked during discovery")
						}
						realm, audience = "https://"+tokenHost+"/token", service
					}
					return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": {`Bearer realm="` + realm + `",service="` + audience + `"`}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				if r.URL.Path == "/token" {
					if r.URL.Host != tokenHost || r.URL.Query().Get("service") != service {
						t.Fatalf("wrong token issuer: %s", r.URL)
					}
					if !reflect.DeepEqual(r.URL.Query()["scope"], []string{"repository:owner/image:pull"}) {
						t.Fatalf("wrong scope: %v", r.URL.Query()["scope"])
					}
					if r.Header.Get("Authorization") != "" || r.URL.Query().Get("account") != "" {
						t.Fatal("Docker Hub credentials leaked to other registry")
					}
					if r.URL.Query().Get("docker_routing") != "" {
						t.Fatal("routing marker leaked")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"token":"registry-token"}`)), Request: r}, nil
				}
				if r.URL.Host != upstream || r.URL.Path != "/v2/owner/image/manifests/test" || r.Header.Get("Authorization") != "Bearer registry-token" {
					t.Fatalf("wrong manifest request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("manifest")), Request: r}, nil
			})}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, httptest.NewRequest("GET", "https://mirror.example/v2/", nil))
			matches := realmRE.FindStringSubmatch(w.Header().Get("WWW-Authenticate"))
			if w.Code != 401 || len(matches) != 2 {
				t.Fatal("root challenge missing")
			}
			u, err := url.Parse(matches[1])
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			q.Set("service", "registry.docker.io")
			q.Set("scope", "repository:"+registry+"/owner/image:pull")
			q.Set("account", "hub-user")
			u.RawQuery = q.Encode()
			r := httptest.NewRequest("GET", u.String(), nil)
			r.SetBasicAuth("hub-user", "hub-secret")
			w = httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("token status %d", w.Code)
			}
			r = httptest.NewRequest("GET", "https://mirror.example/v2/"+registry+"/owner/image/manifests/test", nil)
			r.Header.Set("Authorization", "Bearer registry-token")
			w = httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code != 200 || calls != 4 {
				t.Fatalf("status=%d calls=%d", w.Code, calls)
			}
		})
	}
}

func TestRootTokenScopesRejectMixedRegistries(t *testing.T) {
	c, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rootTokenScopes([]string{"repository:ghcr/owner/image:pull", "repository:quay/owner/image:pull"}, c.Registries); err == nil {
		t.Fatal("mixed registries accepted")
	}
}

func TestUnroutedContainerdDockerHubScopeRemainsUnchanged(t *testing.T) {
	c, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "auth.docker.io" || r.URL.Path != "/token" || r.URL.Query().Get("scope") != "repository:ghcr/owner-image:pull" {
			t.Fatalf("containerd Docker Hub namespace was rerouted: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"token":"hub-token"}`)), Request: r}, nil
	})}
	w := httptest.NewRecorder()
	// Containerd has no root routing marker, even for a namespace named ghcr.
	m.ServeHTTP(w, httptest.NewRequest("GET", "https://mirror.example/auth/dockerhub/aHR0cHM6Ly9hdXRoLmRvY2tlci5pby90b2tlbg?service=registry.docker.io&scope=repository%3Aghcr%2Fowner-image%3Apull", nil))
	if w.Code != 200 {
		t.Fatalf("token status %d", w.Code)
	}
}
