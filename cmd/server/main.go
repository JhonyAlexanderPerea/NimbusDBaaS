package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"

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

	// Determine starting port
	startPort := 8080
	if portStr := os.Getenv("PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil {
			startPort = p
		}
	}

	// Find available port
	port, err := findAvailablePort(startPort)
	if err != nil {
		log.Fatalf("Failed to find an available port: %v", err)
	}

	addr := fmt.Sprintf(":%d", port)
	log.Printf("NimbusDBaaS listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		log.Fatal(err)
	}
}

func findAvailablePort(startPort int) (int, error) {
	for port := startPort; port < startPort+100; port++ {
		addr := fmt.Sprintf(":%d", port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range %d-%d", startPort, startPort+99)
}
