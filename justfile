# krapow — incus-backed GitHub Actions runner manager.
# Run `just` (no args) to see the recipe list.

default:
    @just --list

# build the krapow binary into ./krapow
build:
    go build -o krapow .

# install the binary into ~/.local/bin (must be on PATH)
install: build
    mkdir -p ~/.local/bin
    install -m 0755 krapow ~/.local/bin/krapow
    @echo "installed → ~/.local/bin/krapow"

# remove built artifacts
clean:
    rm -f krapow

# run go test across all packages
test:
    go test ./...

# vet + format check (pre-commit sanity)
lint:
    go vet ./...
    gofmt -l . | grep . && exit 1 || true

# run the preflight diagnostics
doctor: build
    ./krapow doctor

# print krapow runner state
status: build
    ./krapow status

# spawn a fresh Linux runner against owner/name
linux repo: build
    ./krapow init linux --repo {{repo}}

# spawn a fresh Windows runner against owner/name (auto-bakes base image on first run)
win repo: build
    ./krapow init win --repo {{repo}}

# spawn a fresh macOS runner against owner/name (macOS hosts only; uses tart)
mac repo: build
    ./krapow init mac --repo {{repo}}

# destroy a runner by name (tab-completes via shell completion if installed)
destroy name: build
    ./krapow destroy {{name}}

# nuke every krapow-managed runner (state + VM + GitHub) in one go
destroy-all: build
    @./krapow status --quiet 2>/dev/null || true
    @for n in $(ls ~/.krapow/state/ 2>/dev/null | sed 's/\.json$//'); do \
        echo "destroying $n..."; \
        ./krapow destroy "$n"; \
    done

# nuke any stranded bake VMs left over from failed/aborted bake attempts
clean-bakes:
    @for n in $(incus list -c n --format csv 2>/dev/null | grep '^krapow-win-bake-\|^rowner-win-bake-'); do \
        echo "deleting $n..."; \
        incus delete --force "$n"; \
    done

# refresh the bake (deletes the published image + re-runs the full bake)
rebake: build clean-bakes
    incus image delete win-runner-base 2>/dev/null || true
    ./krapow bake
