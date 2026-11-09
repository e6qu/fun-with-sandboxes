package main

import (
	"bufio"
	"log"
	"net"
	"net/http"
	"time"
)

// logRequests logs each request h serves, with its status and how long
// the engine took to serve it, as dockerd's --debug logs the calls it
// serves. A hijacked stream's time is how long the stream stayed open.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		status := sw.status
		switch {
		case sw.hijacked:
			status = http.StatusSwitchingProtocols
		case status == 0:
			status = http.StatusOK
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.RequestURI(), status, time.Since(start).Round(time.Millisecond))
	})
}

// statusWriter records the status a handler writes. It implements
// http.Flusher and http.Hijacker itself, because the API's attach and
// BuildKit's session assert them, and gRPC requires a Flusher.
type statusWriter struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() { http.NewResponseController(w.ResponseWriter).Flush() }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return c, rw, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
