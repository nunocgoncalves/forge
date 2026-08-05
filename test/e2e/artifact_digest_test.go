package e2e

import "regexp"

var canonicalSHA256Digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func isCanonicalSHA256Digest(digest string) bool {
	return canonicalSHA256Digest.MatchString(digest)
}
