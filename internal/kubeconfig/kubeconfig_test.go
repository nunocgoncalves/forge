package kubeconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const sampleKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: Q0VSVElGSUNBVEUK
    insecure-skip-tls-verify: false
contexts:
- name: default
  context:
    cluster: default
    user: default
current-context: default
users:
- name: default
  user:
    client-certificate-data: Q0xJRU5UQ0VSBg==
    client-key-data: Q0xJRU5US0VZ
`

func TestRewriteServer(t *testing.T) {
	out, err := RewriteServer([]byte(sampleKubeconfig), "10.20.0.10", 6443)
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(out, &doc))
	cl := doc["clusters"].([]any)[0].(map[string]any)["cluster"].(map[string]any)
	assert.Equal(t, "https://10.20.0.10:6443", cl["server"])
	// other fields preserved
	assert.Equal(t, "Q0VSVElGSUNBVEUK", cl["certificate-authority-data"])
	assert.Equal(t, false, cl["insecure-skip-tls-verify"])
	// contexts/users/current-context preserved
	assert.NotNil(t, doc["contexts"])
	assert.NotNil(t, doc["users"])
	assert.Equal(t, "default", doc["current-context"])
}

func TestRewriteServer_IPv6(t *testing.T) {
	out, err := RewriteServer([]byte(sampleKubeconfig), "fd00::1", 6443)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(out, &doc))
	cl := doc["clusters"].([]any)[0].(map[string]any)["cluster"].(map[string]any)
	assert.Equal(t, "https://[fd00::1]:6443", cl["server"])
}

func TestRewriteServer_MultipleClusters(t *testing.T) {
	kc := `
clusters:
- name: a
  cluster: {server: https://127.0.0.1:6443}
- name: b
  cluster: {server: https://localhost:6443}
`
	out, err := RewriteServer([]byte(kc), "10.20.0.10", 6443)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(out, &doc))
	for _, c := range doc["clusters"].([]any) {
		s := c.(map[string]any)["cluster"].(map[string]any)["server"]
		assert.Equal(t, "https://10.20.0.10:6443", s)
	}
}

func TestRewriteServer_NoClusters(t *testing.T) {
	_, err := RewriteServer([]byte("apiVersion: v1\nkind: Config\n"), "10.20.0.10", 6443)
	require.Error(t, err)
}

func TestRewriteServer_InvalidYAML(t *testing.T) {
	_, err := RewriteServer([]byte("{"), "10.20.0.10", 6443)
	require.Error(t, err)
}
