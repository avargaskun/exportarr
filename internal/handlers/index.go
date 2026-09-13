package handlers

import (
	"fmt"
	"html"
	"net/http"
	"strings"
)

// IndexHandler serves the landing page linking to /metrics.
func IndexHandler(w http.ResponseWriter, _ *http.Request) {
	response := `<h1>Exportarr</h1><p><a href='/metrics'>metrics</a></p>`
	_, _ = fmt.Fprintln(w, response)
}

// TargetIndexHandler serves a landing page linking to /metrics/<name> for each name, in order.
func TargetIndexHandler(names []string) http.Handler {
	var b strings.Builder
	b.WriteString("<h1>Exportarr</h1><ul>")
	for _, name := range names {
		escaped := html.EscapeString(name)
		b.WriteString("<li><a href='/metrics/" + escaped + "'>" + escaped + "</a></li>")
	}
	b.WriteString("</ul>")
	page := b.String()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
}

// NotFoundHandler answers with a constant 404 that never echoes the request.
func NotFoundHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404 page not found\n"))
}
