package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Registry struct {
	Upstream      string   `json:"upstream"`
	AuthHosts     []string `json:"auth_hosts"`
	RedirectHosts []string `json:"redirect_hosts"`
}
type Config struct {
	Listen     string              `json:"listen"`
	Registries map[string]Registry `json:"registries"`
}
type mirror struct {
	config    Config
	transport http.RoundTripper
}

var realmRE = regexp.MustCompile(`realm="([^"]+)"`)

func allowed(host string, hosts []string) bool {
	for _, h := range hosts {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}
func (m *mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name, path := "dockerhub", r.URL.Path
	var target *url.URL
	var err error
	if strings.HasPrefix(path, "/auth/") {
		parts := strings.Split(strings.TrimPrefix(path, "/auth/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		name = parts[0]
		raw, e := base64.RawURLEncoding.DecodeString(parts[1])
		if e != nil {
			http.Error(w, "invalid auth endpoint", 400)
			return
		}
		target, err = url.Parse(string(raw))
	} else if strings.HasPrefix(path, "/v2/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/v2/"), "/", 2)
		if _, ok := m.config.Registries[parts[0]]; ok && len(parts) == 2 {
			name = parts[0]
			path = "/v2/" + parts[1]
		}
	} else {
		http.NotFound(w, r)
		return
	}
	reg, ok := m.config.Registries[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	auth := target != nil
	if auth {
		if err != nil || target.Scheme != "https" || target.User != nil || !allowed(target.Host, reg.AuthHosts) {
			http.Error(w, "auth endpoint denied", 400)
			return
		}
	} else {
		target, _ = url.Parse(reg.Upstream)
		target.Path = path
	}
	q := target.Query()
	for k, vs := range r.URL.Query() {
		if k != "ns" {
			q[k] = vs
		}
	}
	target.RawQuery = q.Encode()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL = target
			p.Out.Host = target.Host
			p.Out.Header.Del("Forwarded")
			p.Out.Header.Del("X-Forwarded-For")
			p.Out.Header.Del("X-Forwarded-Host")
			p.Out.Header.Del("X-Forwarded-Proto")
			p.Out.Header.Del("X-Forwarded-Port")
			p.Out.Header.Del("X-Real-IP")
			if !auth && strings.HasPrefix(p.Out.Header.Get("Authorization"), "Basic ") {
				p.Out.Header.Del("Authorization")
			}
		},
		Transport: m.transport, FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			// Follow storage redirects on the server: clients need no direct CDN access.
			for hops := 0; resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != ""; hops++ {
				if hops >= 5 {
					return fmt.Errorf("too many redirects")
				}
				next, e := resp.Location()
				if e != nil {
					return e
				}
				if next.Scheme != "https" || next.User != nil || !(next.Host == target.Host || allowed(next.Host, reg.RedirectHosts)) {
					return fmt.Errorf("redirect host denied")
				}
				resp.Body.Close()
				req, e := http.NewRequestWithContext(r.Context(), r.Method, next.String(), nil)
				if e != nil {
					return e
				}
				for _, h := range []string{"Range", "Accept", "If-Range"} {
					if v := r.Header.Get(h); v != "" {
						req.Header.Set(h, v)
					}
				}
				res, e := m.transport.RoundTrip(req)
				if e != nil {
					return e
				}
				*resp = *res
			}
			challenge := resp.Header.Get("WWW-Authenticate")
			if challenge != "" {
				match := realmRE.FindStringSubmatch(challenge)
				if len(match) == 2 {
					u, e := url.Parse(match[1])
					if e != nil || u.Scheme != "https" || !allowed(u.Host, reg.AuthHosts) {
						return fmt.Errorf("auth host denied")
					}
					scheme := "http"
					if r.TLS != nil {
						scheme = "https"
					}
					if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded == "https" || forwarded == "http" {
						scheme = forwarded
					}
					realm := scheme + "://" + r.Host + "/auth/" + name + "/" + base64.RawURLEncoding.EncodeToString([]byte(match[1]))
					resp.Header.Set("WWW-Authenticate", strings.Replace(challenge, match[1], realm, 1))
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			log.Printf("upstream failed registry=%s: %T", name, e)
			http.Error(w, "upstream unavailable", 502)
		},
	}
	proxy.ServeHTTP(w, r)
}
func main() {
	config := flag.String("config", "config.json", "configuration file")
	check := flag.Bool("check", false, "validate configuration and exit")
	flag.Parse()
	data, err := os.ReadFile(*config)
	if err != nil {
		log.Fatal(err)
	}
	var c Config
	if err = json.Unmarshal(data, &c); err != nil {
		log.Fatal(err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if len(c.Registries) == 0 {
		log.Fatal("registries required")
	}
	for name, reg := range c.Registries {
		u, e := url.Parse(reg.Upstream)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
			log.Fatalf("invalid upstream: %s", name)
		}
	}
	if *check {
		fmt.Println("configuration valid")
		return
	}
	t := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 900 * time.Second, DisableCompression: true}
	s := &http.Server{Addr: c.Listen, Handler: &mirror{c, t}, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	log.Printf("opskit-mirror listening on %s (no disk cache)", c.Listen)
	log.Fatal(s.ListenAndServe())
}
