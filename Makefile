.PHONY: build run run-sim tidy clean

BINARY=nimbus-server
CMD=./cmd/server

# Build the binary
build: tidy
	go build -o $(BINARY) $(CMD)/main.go

# Run in simulated mode (no VirtualBox needed)
run-sim:
	NIMBUS_SIMULATED=1 go run $(CMD)/main.go

# Run in real mode (VirtualBox must be configured)
run:
	NIMBUS_SIMULATED=0 go run $(CMD)/main.go

# Download dependencies
tidy:
	go mod tidy

# Clean
clean:
	rm -f $(BINARY) data/nimbus.db
