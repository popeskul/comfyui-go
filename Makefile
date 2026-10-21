.DEFAULT_GOAL := help
.PHONY: help setup comfy run test test-node check tools fmt lint mocks up down logs docker-test

GO_FILES := $(shell find main.go main_test.go internal -name '*.go' -type f)
PY_FILES := $(shell find custom_nodes -name '*.py' -type f)
TOOL_BIN := $(CURDIR)/.tools/bin
GOFUMPT := $(TOOL_BIN)/gofumpt
MOQ := $(TOOL_BIN)/moq
WSL := $(TOOL_BIN)/wsl
RUFF := .tools/venv/bin/ruff
WSL_FLAGS := -enable after-block,after-decl

help:
	@printf '%s\n' \
		'Available commands:' \
		'  make up         Build and start the full Docker stack' \
		'  make down       Stop Docker services (keep data)' \
		'  make logs       Follow Docker service logs' \
		'  make docker-test Run Go and Python tests in Docker' \
		'  make setup      Create local credentials (keeps existing token)' \
		'  make comfy      Start ComfyUI in this terminal' \
		'  make run        Start the Go API in this terminal' \
		'  make fmt        Format Go and Python code' \
		'  make lint       Check formatting and lint rules' \
		'  make mocks      Regenerate interface mocks' \
		'  make test       Run Go tests with race detection' \
		'  make test-node  Run Python node tests (requires .venv)' \
		'  make check      Run lint, Go tests and vet' \
		'  make tools      Install local formatting tools' \
		'  make help       Show this list'

up: setup
	docker compose up --build -d --wait

down:
	docker compose down

logs:
	docker compose logs -f

docker-test: setup
	docker build --target test -t comfyui-go-tests .
	docker run --rm comfyui-go-tests
	docker compose build comfy
	docker compose run --rm --no-deps comfy python -m unittest discover -s custom_nodes/lab_image_adjust -p test_adjust.py -v

setup: .data/token
	@chmod 600 .data/token
	@umask 077; printf 'Authorization: Bearer %s\n' "$$(cat .data/token)" > .data/auth-header
	@umask 077; printf 'API_TOKEN=%s\n' "$$(cat .data/token)" > .data/api.env

.data/token:
	@mkdir -p .data
	@umask 077; openssl rand -hex 32 > $@

comfy:
	@cd ComfyUI && exec ../.venv/bin/python main.py --listen 127.0.0.1 --port 8188 --disable-auto-launch --preview-method none

run: setup
	go build -o .data/image-service .
	@API_TOKEN="$$(cat .data/token)" exec .data/image-service

tools: $(GOFUMPT) $(WSL) $(RUFF)

mocks: $(MOQ) $(GOFUMPT)
	$(MOQ) -out internal/jobs/mocks_test.go ./internal/jobs Backend WorkerStore APIStore JobControl
	$(GOFUMPT) -extra -w internal/jobs/mocks_test.go

$(MOQ):
	mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) go install github.com/matryer/moq@v0.6.0

$(GOFUMPT):
	mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) go install mvdan.cc/gofumpt@v0.9.2

$(WSL):
	mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) go install github.com/bombsimon/wsl/v5/cmd/wsl@v5.9.0

$(RUFF):
	python3 -m venv .tools/venv
	.tools/venv/bin/pip install --disable-pip-version-check ruff==0.16.8

fmt: tools
	$(GOFUMPT) -extra -w $(GO_FILES)
	$(WSL) $(WSL_FLAGS) -fix ./...
	$(RUFF) format $(PY_FILES)
	$(RUFF) check --fix $(PY_FILES)

lint: tools
	@test -z "$$($(GOFUMPT) -extra -l $(GO_FILES))" || { echo 'Run make fmt'; exit 1; }
	$(WSL) $(WSL_FLAGS) ./...
	$(RUFF) check $(PY_FILES)
	$(RUFF) format --check $(PY_FILES)

test:
	go test -race -shuffle=on -count=1 ./...

check: lint test
	go vet ./...
	go mod verify

# Uses the local ComfyUI environment with PyTorch installed.
test-node:
	.venv/bin/python -m unittest discover -s custom_nodes/lab_image_adjust -p test_adjust.py -v
