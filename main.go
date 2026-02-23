package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

//go:embed web/index.html
var webFS embed.FS

func main() {
	router := NewRouter()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// Serve dashboard
	webContent, _ := fs.Sub(webFS, "web")
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, webContent, "index.html")
	})

	// API routes
	r.Route("/v1", func(r chi.Router) {
		r.Post("/payments/authorize", handleAuthorize(router))
		r.Get("/processors", handleListProcessors(router))
		r.Get("/transactions", handleListTransactions(router))

		r.Route("/admin/processors/{id}", func(r chi.Router) {
			r.Post("/simulate", handleSimulate(router))
			r.Post("/status", handleOverrideStatus(router))
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Payment Router starting on http://localhost:%s", port)
	log.Printf("Dashboard: http://localhost:%s/", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
