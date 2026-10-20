// Command campaign-events receives delivery events from providers and serves
// per-campaign stats. See README.md and NOTES.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	data := flag.String("data", "data/events.jsonl", `append-only event log ("" = keep everything in memory only)`)
	flag.Parse()

	store := NewStore()
	if *data != "" {
		if err := os.MkdirAll(filepath.Dir(*data), 0o755); err != nil {
			log.Fatal(err)
		}
		var err error
		if store, err = OpenStore(*data); err != nil {
			log.Fatal(err)
		}
	}
	defer store.Close() // runs because we return from main instead of log.Fatal

	srv := &http.Server{
		Addr:              *addr,
		Handler:           NewServer(store).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	// Ctrl+C / SIGTERM: stop taking new requests, let running ones finish,
	// then close the log file.
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-stop.Done()
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		srv.Shutdown(ctx)
	}()

	fmt.Printf("listening on %s (log file: %q)\n", *addr, *data)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		return
	}
	<-shutdownDone // wait until in-flight requests have finished
	log.Print("shut down cleanly")
}