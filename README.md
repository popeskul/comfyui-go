# Go + ComfyUI

A Go API for image generation with a persistent job queue, duplicate request handling and custom ComfyUI nodes for color adjustment and resizing.

| Original | Portrait |
| --- | --- |
| ![Original](examples/original.png) | ![Portrait](examples/adjusted.png) |

## Start

Requires Git, Docker Compose, Make, OpenSSL and curl. Go and Python run inside Docker. On macOS, start Docker Desktop first.

```sh
git clone https://github.com/popeskul/comfyui-go.git
cd comfyui-go
make up
```

The first build downloads the model and dependencies. Generation runs on CPU. Measured image sizes on Docker Desktop ARM64: 7.9 GB for the services, plus 1.4 GB for Go tests. Build cache and generated files take additional space.

API: `http://127.0.0.1:8088`. ComfyUI: `http://127.0.0.1:8188`. Credentials are created automatically in `.data/`.

## Generate

```sh
curl --fail-with-body http://127.0.0.1:8088/v1/jobs \
  --header @.data/auth-header \
  --header 'Content-Type: application/json' \
  --header 'Idempotency-Key: mug-example-001' \
  --data '{"prompt":"a blue coffee mug on a wooden table","output_format":"portrait"}'
```

Replace `JOB_ID` with the response's `id`. Repeat while `state` is `queued` or `running`. Download only when it is `succeeded`; for `failed` or `uncertain`, check `make logs` and [recovery instructions](OPERATIONS.md#recovery):

```sh
curl --fail-with-body --header @.data/auth-header 'http://127.0.0.1:8088/v1/jobs/JOB_ID'
```

Download the result:

```sh
curl --fail-with-body --header @.data/auth-header \
  'http://127.0.0.1:8088/v1/jobs/JOB_ID/images/1' --output .data/image.png
```

Open `.data/image.png` (on macOS: `open .data/image.png`). Use `/images/0` for the original. Use a new `Idempotency-Key` for each new image; repeating a key reuses the existing job.

Formats: `portrait` is 768x960, `square` is 768x768. Images fit without cropping, with white padding. Omit `output_format` to keep the generated size.

## Commands

Run commands from the repository directory. `make` only prints help.

| Command | What it does |
| --- | --- |
| `make` / `make help` | List all commands. |
| `make up` | Create credentials if needed, build both images and start the Docker stack in the background. |
| `make down` | Stop and remove containers; keep volumes with jobs and images. |
| `make logs` | Follow both services' logs. Ctrl-C exits the viewer without stopping services. |
| `make docker-test` | Build test images and run Go race tests plus Python node tests in Docker. Does not generate images. |
| `make setup` | Create `.data/token`, `.data/auth-header` and `.data/api.env`. Keep the existing token. |
| `make run` | Build and start the Go API locally in the current terminal. Also runs setup. |
| `make comfy` | Start a locally installed ComfyUI in the current terminal. |
| `make test` | Run local Go tests with race detection and shuffled test order. |
| `make test-node` | Run Python node tests using `.venv/`. |
| `make fmt` | Format Go/Python files and apply supported lint fixes. Modifies files. |
| `make lint` | Check Go/Python formatting and lint rules without changing source files. |
| `make check` | Run lint, Go race tests, `go vet` and `go mod verify`. Does not run Python node tests. |
| `make mocks` | Regenerate interface mocks in `internal/jobs/mocks_test.go` using moq. |
| `make tools` | Install pinned gofumpt, wsl and Ruff tools into `.tools/`. |

`make docker-test` uses the same ComfyUI image as the service, so its first build also downloads the model. Tests themselves do not run the model.

### Local development (optional)

Docker users only need `up`, `down`, `logs` and `docker-test`. Local Go commands require Go and a C compiler for SQLite. Formatting commands also require Python 3 with venv/pip; their tools are installed automatically in `.tools/`. No tools are installed globally.

For `make comfy`, install ComfyUI in `ComfyUI/`, its Python dependencies in `.venv/`, the checkpoint in `ComfyUI/models/checkpoints/`, and copy `custom_nodes/lab_image_adjust/` into `ComfyUI/custom_nodes/`. Use the revision, model URL and checksum in `Dockerfile.comfy`. `make test-node` needs PyTorch in `.venv/` but does not need the model.

Stop the Docker stack before running local services on the same ports. Run `make comfy` and `make run` in separate terminals; Ctrl-C stops each process. Local jobs use `.data/jobs.db`; Docker jobs use the `jobs` volume.

## Project layout

- `main.go`: configuration and service lifecycle.
- `internal/jobs/`: API, SQLite queue, ComfyUI client and tests.
- `custom_nodes/`: Python image-processing nodes and tests.
- `workflow.json`: generation and image-processing workflow.
- `compose.yaml`, `Dockerfile`, `Dockerfile.comfy`: Docker services and builds.

See [OPERATIONS.md](OPERATIONS.md) for endpoints, configuration and failed-job recovery. Model source, checksum and ComfyUI revision are in `Dockerfile.comfy`. The model uses the CreativeML Open RAIL-M license.
