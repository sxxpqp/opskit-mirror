package main

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type Registry struct {
	Username    string   `yaml:"username"`
	PasswordEnv string   `yaml:"password_env"`
	Upstream    string   `yaml:"upstream"`
	AuthHosts   []string `yaml:"auth_hosts"`
	// RedirectHosts is accepted for compatibility; redirects now use public-address validation.
	RedirectHosts []string `yaml:"redirect_hosts"`
}
type Config struct {
	Listen     string              `yaml:"listen"`
	Registries map[string]Registry `yaml:"registries"`
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
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
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
	rootRouted := false
	var routedScopes []string
	var routedService string
	if auth && name == "dockerhub" && r.URL.Query().Get("docker_routing") == "1" {
		routedName, scopes, e := rootTokenScopes(r.URL.Query()["scope"], m.config.Registries)
		if e != nil {
			http.Error(w, "mixed registry scopes denied", http.StatusBadRequest)
			return
		}
		// Registries without token authentication (e.g. K8s) ignore the root token.
		if routedName == name || len(m.config.Registries[routedName].AuthHosts) != 0 {
			routedScopes = scopes
			if routedName != name {
				reg = m.config.Registries[routedName]
				target, routedService, e = m.tokenChallenge(r.Context(), reg)
				if e != nil {
					slog.Error("token_routing_failed", "request_id", r.Header.Get("X-Request-ID"), "registry", routedName, "error_type", fmt.Sprintf("%T", e))
					http.Error(w, "upstream unavailable", http.StatusBadGateway)
					return
				}
				name = routedName
				rootRouted = true
			}
		}
	}
	q := target.Query()
	for k, vs := range r.URL.Query() {
		if k != "ns" && k != "repository_prefix" && k != "repository_path" && k != "docker_routing" {
			q[k] = vs
		}
	}
	if auth && r.URL.Query().Get("repository_prefix") == name {
		q["scope"] = upstreamScopes(q["scope"], name, r.URL.Query().Get("repository_path"))
	}
	if routedScopes != nil {
		q["scope"] = routedScopes
	}
	if rootRouted {
		q.Set("service", routedService)
		q.Del("account")
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
			// Docker Hub credentials must not be forwarded to a different registry.
			if rootRouted {
				p.Out.Header.Del("Authorization")
			}
			if auth && reg.Username != "" {
				p.Out.SetBasicAuth(reg.Username, os.Getenv(reg.PasswordEnv))
			}
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
				if !publicRedirectURL(next) {
					slog.Warn("redirect_denied", "request_id", r.Header.Get("X-Request-ID"), "registry", name, "reason", "redirect requires a public HTTPS target without credentials", "redirect_host", next.Hostname(), "redirect_scheme", next.Scheme)
					return fmt.Errorf("redirect host denied")
				}
				resp.Body.Close()
				req, e := http.NewRequestWithContext(r.Context(), r.Method, next.String(), nil)
				if e != nil {
					return e
				}
				for _, h := range []string{"Range", "Accept", "If-Range"} {
					if values := r.Header.Values(h); len(values) != 0 {
						req.Header[h] = append([]string(nil), values...)
					}
				}
				res, e := m.transport.RoundTrip(req)
				if e != nil {
					return e
				}
				*resp = *res
			}
			challenge := resp.Header.Get("WWW-Authenticate")
			if !auth && name == "ghcr" && path == "/v2/" && r.URL.Query().Get("ns") == "" {
				// A registry probe is not a request for user/image. Let the
				// client supply its real repository scope when asking for a token.
				challenge = probeScopeRE.ReplaceAllString(challenge, "")
			}
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
					// Docker includes the routing prefix in its repository scope; CRI uses the original name.
					if !auth && r.URL.Query().Get("ns") == "" && strings.HasPrefix(r.URL.Path, "/v2/"+name+"/") {
						realm += "?repository_prefix=" + url.QueryEscape(name) + "&repository_path=" + url.QueryEscape(manifestRepository(path))
					}
					if !auth && r.URL.Path == "/v2/" && r.URL.Query().Get("ns") == "" {
						realm += "?docker_routing=1"
					}
					resp.Header.Set("WWW-Authenticate", strings.Replace(challenge, match[1], realm, 1))
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			reason := "upstream request failed"
			if errors.Is(e, errNonPublicAddress) {
				reason = "destination address is not public"
			}
			slog.Error("upstream_failed", "request_id", r.Header.Get("X-Request-ID"), "registry", name, "error_type", fmt.Sprintf("%T", e), "reason", reason)
			http.Error(w, "upstream unavailable", 502)
		},
	}
	proxy.ServeHTTP(w, r)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	config := flag.String("config", "", "optional YAML file; defaults to embedded config.example.yaml")
	check := flag.Bool("check", false, "validate configuration and exit")
	flag.Parse()
	c, err := loadConfig(*config)
	if err != nil {
		log.Fatal(err)
	}
	if len(c.Registries) == 0 {
		log.Fatal("registries required")
	}
	for name, reg := range c.Registries {
		if (reg.Username == "") != (reg.PasswordEnv == "") {
			log.Fatalf("username and password_env must be configured together: %s", name)
		}
		if reg.PasswordEnv != "" && os.Getenv(reg.PasswordEnv) == "" {
			log.Fatalf("credential environment variable is empty: %s", reg.PasswordEnv)
		}
		u, e := url.Parse(reg.Upstream)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
			log.Fatalf("invalid upstream: %s", name)
		}
	}
	if *check {
		fmt.Println("configuration valid")
		return
	}
	t := &http.Transport{DialContext: publicDialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 900 * time.Second, DisableCompression: true}
	s := &http.Server{Addr: c.Listen, Handler: accessLog(&mirror{c, t}), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	log.Printf("opskit-mirror listening on %s (no disk cache)", c.Listen)
	log.Fatal(s.ListenAndServe())
}

//go:embed config.example.yaml
var defaultConfig []byte

//go:embed index.html
var indexHTML []byte

// Precedence: embedded defaults < explicit YAML < environment.
func loadConfig(path string) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(defaultConfig, &c); err != nil {
		return c, err
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		var file Config
		if err := yaml.Unmarshal(data, &file); err != nil {
			return c, err
		}
		if file.Listen != "" {
			c.Listen = file.Listen
		}
		if file.Registries != nil {
			c.Registries = file.Registries
		}
	}
	if value, ok := os.LookupEnv("MIRROR_LISTEN"); ok {
		c.Listen = value
	}
	for name, reg := range c.Registries {
		prefix := "MIRROR_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
		for key, dst := range map[string]*string{"UPSTREAM": &reg.Upstream, "USERNAME": &reg.Username, "PASSWORD_ENV": &reg.PasswordEnv} {
			if value, ok := os.LookupEnv(prefix + key); ok {
				*dst = value
			}
		}
		for key, dst := range map[string]*[]string{"AUTH_HOSTS": &reg.AuthHosts, "REDIRECT_HOSTS": &reg.RedirectHosts} {
			if value, ok := os.LookupEnv(prefix + key); ok {
				*dst = nil
				for _, host := range strings.Split(value, ",") {
					if host = strings.TrimSpace(host); host != "" {
						*dst = append(*dst, host)
					}
				}
			}
		}
		// A direct password/token environment variable needs no password_env setting.
		if value, ok := os.LookupEnv(prefix + "PASSWORD"); ok && (value != "" || reg.Username != "") {
			reg.PasswordEnv = prefix + "PASSWORD"
		}
		c.Registries[name] = reg
	}
	if c.Listen == "" {
		return c, fmt.Errorf("listen must not be empty")
	}
	return c, nil
}

var requestSequence atomic.Uint64

type accessWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *accessWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *accessWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		id := fmt.Sprintf("%x-%x", start.UnixNano(), requestSequence.Add(1))
		r.Header.Set("X-Request-ID", id)
		w.Header().Set("X-Request-ID", id)
		registry, operation := "dockerhub", "registry"
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			operation = "token"
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) > 2 {
				registry = parts[2]
			}
		} else if strings.HasPrefix(r.URL.Path, "/v2/") {
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v2/"), "/", 2)
			if len(parts) == 2 {
				if m, ok := next.(*mirror); ok {
					if _, exists := m.config.Registries[parts[0]]; exists {
						registry = parts[0]
					}
				}
			}
			if strings.Contains(r.URL.Path, "/blobs/") {
				operation = "blob"
			} else if strings.Contains(r.URL.Path, "/manifests/") {
				operation = "manifest"
			}
		}
		out := &accessWriter{ResponseWriter: w}
		next.ServeHTTP(out, r)
		if out.status == 0 {
			out.status = 200
		}
		result := "ok"
		if out.status >= 400 {
			result = "error"
		}
		if r.Context().Err() != nil {
			result = "client_canceled"
		}
		slog.Info("request_completed", "request_id", id, "registry", registry, "operation", operation, "method", r.Method, "status", out.status, "bytes", out.bytes, "duration_ms", time.Since(start).Milliseconds(), "result", result)
	})
}
