package main

import "strings"

// Translate Docker's routed repository names back to upstream names. A token
// request may contain both the challenge scope and Docker's own repository scope.
func upstreamScopes(scopes []string, prefix, repository string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, value := range scopes {
		for _, scope := range strings.Fields(value) {
			parts := strings.SplitN(scope, ":", 3)
			if len(parts) == 3 && parts[0] == "repository" && repository != "" && parts[1] == prefix+"/"+repository {
				parts[1] = repository
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
