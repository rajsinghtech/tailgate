package main

import (
	"net/http"
)

// listenAndServe wraps http.ListenAndServe so tests can swap it out.
var listenAndServe = func(addr string, h http.Handler) error {
	return http.ListenAndServe(addr, h)
}
