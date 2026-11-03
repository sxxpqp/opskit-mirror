package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Translate Docker's routed repository names back to upstream names. A token
// request may contain both the challenge scope and Docker's own repository scope.
func upstreamScopes(scopes []string, prefix, repository string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, value := range scopes {
		for _, scope := range strings.Fields(value) {
			parts := strings.SplitN(scope, ":", 3)
			if len(parts) == 3 && parts[0] == "repository" &&
				((repository != "" && parts[1] == prefix+"/"+repository) ||
					(repository == "" && strings.HasPrefix(parts[1], prefix+"/"))) {
				if repository == "" {
					// A Docker /v2/<registry>/ probe has no repository path.
					// Its client-supplied scope still includes one routing prefix.
					parts[1] = strings.TrimPrefix(parts[1], prefix+"/")
				} else {
					parts[1] = repository
				}
				scope = strings.Join(parts, ":")
			}
			if !seen[scope] {
				result = append(result, scope)
				seen[scope] = true
			}
		}
	}
	return result
}

func manifestRepository(path string) string {
	for _, operation := range []string{"/manifests/", "/blobs/", "/tags/"} {
		if index := strings.LastIndex(path, operation); index >= 0 {
			return strings.TrimPrefix(path[:index], "/v2/")
		}
	}
	return ""
}

var serviceRE = regexp.MustCompile(`service="([^"]+)"`)

// GHCR probes advertise repository:user/image:pull without a real repository.
// Remove the whole scope parameter, regardless of its position in the challenge.
var probeScopeRE = regexp.MustCompile(`,\s*scope="[^"]*"|scope="[^"]*",\s*`)

// Classic Docker authenticates against /v2/, before it requests a repository.
// Select the token issuer from its routed repository scopes instead of issuing
// a Docker Hub token for a GHCR/Quay resource.
func rootTokenScopes(scopes []string, registries map[string]Registry) (string, []string, error) {
	selected := ""
	var normalized []string
	for _, value := range scopes {
		for _, scope := range strings.Fields(value) {
			parts := strings.SplitN(scope, ":", 3)
			if len(parts) != 3 || parts[0] != "repository" {
				normalized = append(normalized, scope)
				continue
			}
			name := "dockerhub"
			prefix, repository, hasPrefix := strings.Cut(parts[1], "/")
			if _, ok := registries[prefix]; ok && hasPrefix && repository != "" {
				name = prefix
				parts[1] = repository
			}
			if selected != "" && selected != name {
				return "", nil, fmt.Errorf("mixed registry scopes denied")
			}
			selected = name
			normalized = append(normalized, strings.Join(parts, ":"))
		}
	}
	if selected == "" {
		selected = "dockerhub"
	}
	return selected, normalized, nil
}

func (m *mirror) tokenChallenge(ctx context.Context, reg Registry) (*url.URL, string, error) {
	u, err := url.Parse(reg.Upstream)
	if err != nil {
		return nil, "", fmt.Errorf("invalid registry upstream")
	}
	u.Path = "/v2/"
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := m.transport.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	challenge := resp.Header.Get("WWW-Authenticate")
	realm := realmRE.FindStringSubmatch(challenge)
	service := serviceRE.FindStringSubmatch(challenge)
	if resp.StatusCode != http.StatusUnauthorized || len(realm) != 2 || len(service) != 2 {
		return nil, "", fmt.Errorf("registry token challenge unavailable")
	}
	target, err := url.Parse(realm[1])
	if err != nil || target.Scheme != "https" || target.User != nil || !allowed(target.Host, reg.AuthHosts) {
		return nil, "", fmt.Errorf("auth host denied")
	}
	return target, service[1], nil
}

