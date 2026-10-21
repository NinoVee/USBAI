.PHONY: test vet drive drive-full drive-pro drive-pro-full run

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

# The Pro edition for the large drive (cloud models), in dist/GANG-AI-PRO.
drive-pro:
	scripts/build-drive.sh --pro

drive-pro-full:
	scripts/build-drive.sh --pro --fetch

# Run from source against dist/PRIVATE-AI.
run:
	go run ./cmd/privateai -drive dist/PRIVATE-AI
