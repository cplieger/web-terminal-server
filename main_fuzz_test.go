package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzBasicAuthRejectsWrongCredentials asserts the auth gate security
// invariant: only the exact configured (username, password) pair is
// accepted and every other pair must receive 401. HTTP Basic encodes
// "user:pass" and splits on the first colon, so the only input whose
// decoded form re-parses to the accepted pair is that pair itself.
//
// It drives basicAuthGate.middleware, the 401 half of the gate whose
// presentsValidCredentials predicate the failed-auth throttle also reads — so
// every pair this proves rejected is also a pair the throttle charges a token
// for, by construction rather than by a second implementation.
func FuzzBasicAuthRejectsWrongCredentials(f *testing.F) {
	const user, pass = "admin", "s3cret"
	f.Add("admin", "wrong")
	f.Add("root", "s3cret")
	f.Add("", "")
	f.Add("admin", "")
	f.Add("admin\x00", "s3cret")
	f.Add("admin", "s3cret ")
	f.Fuzz(func(t *testing.T, inUser, inPass string) {
		if inUser == user && inPass == pass {
			return // the one accepted pair; covered by the unit test
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth(inUser, inPass)
		basicAuth := newBasicAuthGate(user, pass).middleware(next)
		basicAuth.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("basicAuth accepted non-matching credentials (user=%q, pass=%q): status=%d, want 401",
				inUser, inPass, rec.Code)
		}
	})
}
