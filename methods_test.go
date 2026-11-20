package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMethodWrapperAllowsReadOnlyMethods(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "PROPFIND"} {
		t.Run(method, func(t *testing.T) {
			called := false
			h := methodWrapper(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, "/file", nil))
			if !called {
				t.Fatalf("%s was not passed to the WebDAV handler", method)
			}
			if rec.Code != http.StatusNoContent {
				t.Fatalf("%s status = %d, want %d", method, rec.Code, http.StatusNoContent)
			}
		})
	}
}

func TestMethodWrapperOptions(t *testing.T) {
	called := false
	h := methodWrapper(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Allow"); got != allowHeader {
		t.Errorf("Allow = %q, want %q", got, allowHeader)
	}
	if got := rec.Header().Get("DAV"); got != "1" {
		t.Errorf("DAV = %q, want %q", got, "1")
	}
	if called {
		t.Error("OPTIONS reached the WebDAV handler")
	}
}

func TestMethodWrapperRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{"PUT", "POST", "DELETE", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "PROPPATCH", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			called := false
			h := methodWrapper(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, "/file", nil))

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
			}
			if got := rec.Header().Get("Allow"); got != allowHeader {
				t.Errorf("%s Allow = %q, want %q", method, got, allowHeader)
			}
			if called {
				t.Errorf("%s reached the WebDAV handler", method)
			}
		})
	}
}

func TestMethodWrapperLeavesAuthInFront(t *testing.T) {
	h := withBasicAuth("user", "pass", methodWrapper(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

	// An unauthenticated write must look unauthorized, not method-not-allowed:
	// auth wraps the method check, not the other way around.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PUT", "/file", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req := httptest.NewRequest("PUT", "/file", nil)
	req.SetBasicAuth("user", "pass")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("authenticated PUT status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
