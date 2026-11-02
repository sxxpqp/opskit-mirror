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
