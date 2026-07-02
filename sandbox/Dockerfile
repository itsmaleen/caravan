# ── stage 1: build caravan from source ───────────────────────────────────────
FROM golang:1.24-bookworm AS builder

WORKDIR /src

# Fetch deps first so this layer is cached between code-only changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source and build a fully-static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /caravan .

# ── stage 2: slim runtime image ───────────────────────────────────────────────
FROM debian:bookworm-slim

# git is required because caravan up shells out to it.
# ca-certificates lets git clone https:// repos.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Drop the static binary into a standard location.
COPY --from=builder /caravan /usr/local/bin/caravan

# Bake the manifest so `caravan up` finds it at the default path on every run.
# The manifest itself lives in sandbox/caravan.toml in the build context.
COPY sandbox/caravan.toml /root/.config/caravan/caravan.toml

# ── age key injection (optional) ─────────────────────────────────────────────
# If your manifest references a secrets file you have two options:
#
# 1. Runtime mount (recommended — key never baked into image layers):
#      docker run --rm \
#        -v "$HOME/.config/caravan/age.key:/root/.config/caravan/age.key:ro" \
#        <image>
#
# 2. Build-time secret (key available only during build, not in image):
#      docker build --secret id=agekey,src="$HOME/.config/caravan/age.key" \
#        -t <image> .
#    Then in a RUN step:
#      RUN --mount=type=secret,id=agekey,dst=/root/.config/caravan/age.key \
#          caravan up --dry-run   # or bake workspace state during build
#
# The test manifest (sandbox/caravan.toml) clones two public repos and needs
# no secrets, so neither option is required for the basic recipe.

# Every `docker run` is a fresh container — provisioning at start IS the flow.
ENTRYPOINT ["/usr/local/bin/caravan", "up"]
