package main

import (
	"net/http"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/httpwire"
)

// codeSpecs are the codes this app answers that no library lists;
// httpservertest.CheckCodeSpecs holds them to the source.
var codeSpecs = map[httpserver.ErrorCode]httpserver.CodeSpec{
	"non_canonical_path": {Status: http.StatusBadRequest, Remediation: "Send the route path exactly as documented, without empty, dot or dot-dot segments.", NoEffect: true},
}

// catalog is every code the listener answers, with its status and
// remediation. Its codes are program data, so a refusal to add one is a bug
// in this file and stops the process at start.
var catalog = func() *httpwire.Catalog {
	c := httpwire.NewCatalog()
	for code, spec := range codeSpecs {
		if err := c.Add(string(code), spec); err != nil {
			panic(err)
		}
	}
	for code, words := range map[string]string{
		"host_not_allowed": "Add the host name you browse to to ALLOWED_HOSTS, or browse to one it lists.",
		"rate_limited":     "Wait for the Retry-After delay, then open the terminal again.",
	} {
		if err := c.SetRemediation(code, words); err != nil {
			panic(err)
		}
	}
	return c
}()

// errorResponder writes every refusal the app's own layers answer, at the
// catalog's status and with its remediation.
var errorResponder = catalog.Responder()
