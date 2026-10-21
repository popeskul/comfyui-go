# Go + ComfyUI

Go handles HTTP requests and a durable SQLite job queue. ComfyUI generates images with Stable Diffusion 1.5. Two small custom Python nodes adjust color and fit the image into a catalog format without cropping.

| Original | Catalog portrait |
| --- | --- |
| ![Original](examples/original.png) | ![Catalog portrait](examples/adjusted.png) |

## Setup

Install Go, a C compiler and [ComfyUI](https://docs.comfy.org/installation/manual_install). Keep ComfyUI in `ComfyUI/` and its Python environment in `.venv/`. Put `v1-5-pruned-emaonly-fp16.safetensors` from the [Comfy-Org archive](https://huggingface.co/Comfy-Org/stable-diffusion-v1-5-archive) in `ComfyUI/models/checkpoints/`. Copy `custom_nodes/lab_image_adjust/` into `ComfyUI/custom_nodes/`.

The tested ComfyUI revision is `7a28332047b6af638dc6c7f72ef3481a718769fd`. The checkpoint SHA-256 is `e9476a13728cd75d8279f6ec8bad753a66a1957ca375a1464dc63b37db6e3916`, licensed under CreativeML Open RAIL-M. Models and runtime dependencies are installed separately.

## Run

Create local credentials once:

```sh
make setup
```

Start ComfyUI in one terminal:

```sh
make comfy
```

Start the Go API in another:

```sh
make run
```

The API listens on `http://127.0.0.1:8088`. Each process runs in the foreground with visible logs; stop each with Ctrl-C. `make run` also runs setup if needed. Repeating setup keeps the existing token. Credentials and the database stay in ignored `.data/`; nothing is configured globally.

Submit a job from another terminal:

```sh
curl --fail-with-body http://127.0.0.1:8088/v1/jobs \
  --header @.data/auth-header \
  --header 'Content-Type: application/json' \
  --header 'Idempotency-Key: mug-example-001' \
  --data '{"prompt":"a blue coffee mug on a wooden table","output_format":"portrait"}'
```

Use the returned job ID to check its status. When it succeeds, download either image with index 0 or 1:

```sh
curl --fail-with-body --header @.data/auth-header 'http://127.0.0.1:8088/v1/jobs/JOB_ID'
curl --fail-with-body --header @.data/auth-header 'http://127.0.0.1:8088/v1/jobs/JOB_ID/images/1' --output .data/image.png
```

Repeating the same request with the same key returns the existing job. Use a new key for a new image.

`output_format` accepts `square` (768x768) or `portrait` (768x960). The whole image is resized with its proportions preserved and centered on a white canvas. Omit this field to keep the generated dimensions. Resizing uses bilinear interpolation. Image 0 is the original; image 1 is the corrected and formatted result.

The workflow is: generation -> color adjustment -> catalog frame -> save. Both custom nodes and their tests live in the same small Python package.

## Checks

Run `make` to list available commands.

```sh
make fmt
make check
make test-node
```

`make check` runs formatting checks, Go race tests and vet without a GPU. `make test-node` uses PyTorch from `.venv/` to test the Python node. Formatting tools install locally in `.tools/`. CI checks the Go service and builds its Docker image.

Mocks in `internal/jobs/mocks_test.go` are generated from interfaces with moq. Run `make mocks` after changing an interface; do not edit the generated file. SQLite and HTTP adapter tests use a real temporary database and a local test server.

## Structure

- `main.go`: configuration and process lifecycle.
- `internal/jobs/`: API, SQLite queue, ComfyUI client and tests.
- `custom_nodes/`: Python image adjustment and its test.
- `workflow.json`: the single generation workflow.
- [OPERATIONS.md](OPERATIONS.md): endpoints, configuration and recovery.

The Dockerfile builds the Go service; ComfyUI runs separately. One service process owns the SQLite database. Jobs survive restarts, and an ambiguous upstream submission is never retried automatically.
