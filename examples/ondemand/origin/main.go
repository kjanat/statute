// The demo origin returns request bytes in a JSON/base64 envelope so the
// cold-start example can verify its payload without reflecting raw HTML.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	log.Fatal(originServer().ListenAndServe())
}

func originServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ready\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read request body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := json.NewEncoder(w).Encode(struct {
			Body []byte `json:"body"`
		}{Body: body}); err != nil {
			log.Printf("write echo response: %v", err)
		}
	})
	return &http.Server{
		Addr: ":7000", Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
	}
}
