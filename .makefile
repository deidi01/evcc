# Makefile (im evcc Projektverzeichnis auf dem Laptop)

PI_HOST   = ibd@192.168.42.7
PI_PATH   = /home/docker/evcc
PI_BINARY = evcc-custom

# Pi 4/5 (64-bit) – anpassen falls nötig
GOOS      = linux
GOARCH    = arm64

.PHONY: build deploy restart logs

## Kompilieren für den Pi
build:
	@echo "→ Cross-compiling für $(GOARCH)..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags="-s -w" \
		-o $(PI_BINARY) \
		./cmd/evcc
	@echo "✓ Binary: $(PI_BINARY) ($$(du -sh $(PI_BINARY) | cut -f1))"

## Binary auf den Pi kopieren
deploy: build
	@echo "→ Deploying zu $(PI_HOST):$(PI_PATH)/..."
	ssh $(PI_HOST) "mkdir -p $(PI_PATH) && systemctl stop evcc 2>/dev/null || true"
	scp $(PI_BINARY) $(PI_HOST):$(PI_PATH)/evcc
	@echo "✓ Deployed"

## evcc auf dem Pi neu starten
restart:
	ssh $(PI_HOST) "cd $(PI_PATH) && sudo systemctl start evcc || ./evcc --config evcc.yaml"

## Logs des Pi live verfolgen
logs:
	ssh $(PI_HOST) "journalctl -u evcc -f --no-pager 2>/dev/null || tail -f $(PI_PATH)/evcc.log"

## Alles in einem: build + deploy + restart + logs
run: deploy restart logs

## Nur OCPP2-Package bauen (schneller Syntax-Check)
check:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build ./charger/ocpp2/...
	@echo "✓ ocpp2 package OK"