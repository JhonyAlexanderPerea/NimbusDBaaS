package main

import (
	"log"
	"net/http"
	"os"

	"nimbusDBaaS/internal/api"
	"nimbusDBaaS/internal/store"
)

func main() {
	// Ensure data directory exists
	if err := os.MkdirAll("data", 0755); err != nil {
		log.Fatalf("Failed to create data dir: %v", err)
	}

	// Initialize store
	db, err := store.NewStore("data/nimbus.db")
	if err != nil {
		log.Fatalf("Failed to open store: %v", err)
	}
	defer db.Close()

	// Build router
	router := api.NewRouter(db)

	addr := ":8080"
	log.Printf("NimbusDBaaS listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		log.Fatal(err)
	}
}
