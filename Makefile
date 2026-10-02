.PHONY: test vet drive drive-full run

test:
	go test ./...

vet:
	go vet ./...

# Binaries, launchers and config in dist/PRIVATE-AI.
drive:
	scripts/build-drive.sh

# Same, plus llama.cpp runtimes and models (several GB of downloads).
drive-full:
	scripts/build-drive.sh --fetch

# Run from source against dist/PRIVATE-AI.
run:
	go run ./cmd/privateai -drive dist/PRIVATE-AI
