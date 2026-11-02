package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicAddresses(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"168.63.129.16", "169.254.169.254", "100.100.100.200", "0.0.0.0", "224.0.0.1",
		"192.0.0.1", "198.18.0.1", "192.0.2.1", "240.0.0.1",
		"::1", "::", "fc00::1", "fe80::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a00:1",
		"2002:7f00:1::", "2001:db8::1", "3fff::1",
	} {
		if publicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("allowed non-public address %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "142.250.142.82", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("blocked public address %s", raw)
		}
	}
}

func TestDialPublicPinsDNSAndBlocksPrivateAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []netip.Addr
		blocked bool
	}{
		{"public", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, false},
		{"private", []netip.Addr{netip.MustParseAddr("10.0.0.1")}, true},
		{"mixed", []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups, dials := 0, 0
			lookup := func(_ context.Context, network, host string) ([]netip.Addr, error) {
				lookups++
				if network != "ip" || host != "cdn.example" {
					t.Fatalf("unexpected lookup %s %s", network, host)
				}
				return tc.answers, nil
			}
			dial := func(_ context.Context, network, address string) (net.Conn, error) {
				dials++
				if network != "tcp" || address != "8.8.8.8:443" {
					t.Fatalf("not pinned to validated IP: %s %s", network, address)
				}
				return nil, nil
			}
			_, err := dialPublic(context.Background(), "tcp", "cdn.example:443", lookup, dial)
			if lookups != 1 {
				t.Fatalf("lookups = %d", lookups)
			}
			if tc.blocked {
				if !errors.Is(err, errNonPublicAddress) || dials != 0 {
					t.Fatalf("private answer reached dial: %d, %v", dials, err)
				}
			} else if err != nil || dials != 1 {
				t.Fatalf("public dial: %d, %v", dials, err)
			}
		})
	}
	_, err := dialPublic(context.Background(), "tcp", "[::ffff:169.254.169.254]:443",
		func(context.Context, string, string) ([]netip.Addr, error) {
			t.Fatal("literal IP must not resolve")
			return nil, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("metadata IP must not dial")
			return nil, nil
		})
	if !errors.Is(err, errNonPublicAddress) {
		t.Fatal(err)
	}
}

func TestRedirectPublicCDNAndCredentialIsolation(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, registry := range []string{"k8s", "quay"} {
			t.Run(method+registry, func(t *testing.T) {
				c, err := loadConfig("")
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://new-cdn.example/download?signature=keep-me"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
					}
					if calls != 2 || r.URL.Host != "new-cdn.example" || r.URL.RawQuery != "signature=keep-me" || r.Method != method {
						t.Fatalf("unexpected redirect: %s %s", r.Method, r.URL)
					}
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Fatal("credentials leaked to CDN")
					}
					if r.Header.Get("Range") != "bytes=0-99" || r.Header.Get("Accept") != "application/octet-stream" || r.Header.Get("If-Range") != "test-etag" {
						t.Fatal("download headers lost")
					}
					return &http.Response{StatusCode: 206, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				})}
				r := httptest.NewRequest(method, "https://mirror.example/v2/"+registry+"/image/blobs/sha256:test", nil)
				r.Header.Set("Authorization", "Bearer secret")
				r.Header.Set("Cookie", "session=secret")
				r.Header.Set("Range", "bytes=0-99")
				r.Header.Set("Accept", "application/octet-stream")
				r.Header.Set("If-Range", "test-etag")
				w := httptest.NewRecorder()
				m.ServeHTTP(w, r)
				if w.Code != 206 || calls != 2 {
					t.Fatalf("status=%d calls=%d", w.Code, calls)
				}
			})
		}
	}
}

func TestRedirectRestrictions(t *testing.T) {
	for _, location := range []string{"http://cdn.example/blob", "https://user:password@cdn.example/blob", "https://127.0.0.1/blob", "https://169.254.169.254/blob", "https://[::1]/blob"} {
		t.Run(location, func(t *testing.T) {
			c, err := loadConfig("")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 307, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, httptest.NewRequest("GET", "https://mirror.example/v2/k8s/image/blobs/sha256:test", nil))
			if w.Code != 502 || calls != 1 {
				t.Fatalf("blocked redirect reached transport: status=%d calls=%d", w.Code, calls)
			}
		})
	}
}

func TestRedirectLimit(t *testing.T) {
	c, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := &mirror{c, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://cdn.example/loop"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "https://mirror.example/v2/k8s/image/manifests/test", nil))
	if w.Code != 502 || calls != 6 {
		t.Fatalf("status=%d calls=%d; expected initial request and 5 redirects", w.Code, calls)
	}
}
