package main

import "net/http"

// allowHeader lists every method the read-only share supports.
const allowHeader = "GET, HEAD, OPTIONS, PROPFIND"

// methodWrapper serves only read-only WebDAV methods. It answers OPTIONS itself
// so clients see the same methods the wrapper actually allows.
func methodWrapper(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, "PROPFIND":
			h.ServeHTTP(w, r)
		case http.MethodOptions:
			w.Header().Set("Allow", allowHeader)
			// Class 2 would promise LOCK/UNLOCK, which the wrapper refuses.
			w.Header().Set("DAV", "1")
			w.Header().Set("MS-Author-Via", "DAV")
		default:
			w.Header().Set("Allow", allowHeader)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		}
	})
}
