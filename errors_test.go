package main

import (
	"maps"
	"testing"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/httpserver/v2/httpservertest"
)

// The package writes exactly codeSpecs (the throttles pass their codes to
// httpserver options, which CheckCodeSpecs does not count), and every code its
// layers answer reaches the client with a remediation.
func TestCodeSpecs_matchSource(t *testing.T) {
	written := maps.Clone(codeSpecs)
	httpservertest.CheckCodeSpecs(t, ".", written)
	for _, code := range []httpserver.ErrorCode{"non_canonical_path", "host_not_allowed", "rate_limited", "too_many_auth_failures", "cross_origin_refused", "method_not_allowed", "internal_error"} {
		if spec, ok := catalog.Lookup(code); !ok || spec.Remediation == "" {
			t.Errorf("catalog has no remediation for %q", code)
		}
	}
}
