// Command wfserver serves the window-function workbench web page.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"dragonboat/windowengine/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           web.NewServer(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("window engine workbench listening on http://%s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
