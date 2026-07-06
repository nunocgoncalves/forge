// Package kubeconfig rewrites a fetched kubeconfig for off-host use.
package kubeconfig

import (
	"fmt"
	"net"
	"strconv"

	"gopkg.in/yaml.v3"
)

// RewriteServer rewrites every cluster's server URL to https://<host>:<port> so
// the kubeconfig is usable from outside the host. All other fields (certs,
// contexts, users, current-context) are preserved.
func RewriteServer(data []byte, host string, port int) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	clusters, ok := doc["clusters"].([]any)
	if !ok {
		return nil, fmt.Errorf("kubeconfig has no clusters")
	}
	server := "https://" + net.JoinHostPort(host, strconv.Itoa(port))
	for _, c := range clusters {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cl, ok := cm["cluster"].(map[string]any); ok {
			cl["server"] = server
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal kubeconfig: %w", err)
	}
	return out, nil
}
