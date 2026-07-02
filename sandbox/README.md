# caravan sandbox recipe

Every `docker run` is a fresh container. `caravan up` runs at container start
and provisions the full workspace from the manifest baked into the image.
That is the flow being validated — cold-start = fresh container = provision happens.

## Files

| File | Purpose |
|---|---|
| `Dockerfile` | Multi-stage build: Go builder → debian:bookworm-slim runtime |
| `caravan.toml` | Test manifest — clones two public octocat repos into /root/code |
| `devcontainer.json` | VS Code / Codespaces devcontainer spec |

## Build

Build from the caravan repo root (the Dockerfile expects the module source
in the build context and `sandbox/caravan.toml` for the runtime manifest):

```
docker build -f sandbox/Dockerfile -t caravan-sandbox:local .
```

Image size: ~282 MB (debian:bookworm-slim + git + ~8 MB static caravan binary).

## Run

```
docker run --rm caravan-sandbox:local
```

Each run is a cold start — a fresh container with no prior workspace. Output:

```
  NAME         ACTION  BRANCH  DETAIL
✓ hello-world  cloned  master
✓ spoon-knife  cloned  main

took 795ms
```

Measured cold-start (wall time of `docker run`, including container setup):
**~1.3 seconds** for two public repos. Well within the 5-minute target from PLAN.md.

To verify the workspace after provisioning:

```
docker run --rm --entrypoint /bin/bash caravan-sandbox:local \
  -c "caravan up && ls /root/code"
```

## Age key injection (for manifests with secrets)

The test manifest has no `[secrets]` block, so no key is needed.
For real manifests that reference a `secrets.enc.json` file, inject the key at
**runtime** so it never touches an image layer:

```
# Recommended: runtime bind-mount (key not baked into image)
docker run --rm \
  -v "$HOME/.config/caravan/age.key:/root/.config/caravan/age.key:ro" \
  caravan-sandbox:local
```

Alternative — build-time secret (available only during build, not in the
final image). Uncomment the `RUN --mount=type=secret` block in the Dockerfile
and build with:

```
docker build \
  --secret id=agekey,src="$HOME/.config/caravan/age.key" \
  -f sandbox/Dockerfile -t caravan-sandbox:local .
```

The runtime mount is the recommended approach: the key never appears in
image history, no special BuildKit flags are required at run time, and the
same image works for any operator who has the key.

## devcontainer usage

Open the caravan repo in VS Code and choose "Dev Containers: Reopen in
Container". The container builds from `sandbox/Dockerfile` and runs
`caravan up` via `postCreateCommand`. The workspace is present before the
editor opens.

To inject an age key in devcontainers, add a `mounts` entry to
`devcontainer.json`:

```json
"mounts": [
  "source=${localEnv:HOME}/.config/caravan/age.key,target=/root/.config/caravan/age.key,type=bind,readonly"
]
```
