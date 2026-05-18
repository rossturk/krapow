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

# tag and push v<version>; the release workflow takes it from there.
# usage: just release 0.2.0
release version:
    #!/usr/bin/env bash
    set -euo pipefail

    tag="v{{version}}"

    # Refuse to tag in messy states. Each of these has bitten me at least
    # once on other projects — better to fail loud and local than discover
    # mid-release that the artifact doesn't match what's in main.
    if [[ ! "{{version}}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
        echo "error: '{{version}}' isn't semver (expected MAJOR.MINOR.PATCH[-pre])" >&2
        exit 1
    fi
    if [[ -n "$(git status --porcelain)" ]]; then
        echo "error: working tree not clean — commit or stash first" >&2
        git status --short >&2
        exit 1
    fi
    branch=$(git rev-parse --abbrev-ref HEAD)
    if [[ "$branch" != "main" ]]; then
        echo "error: must be on main (currently '$branch')" >&2
        exit 1
    fi
    git fetch --quiet origin main
    if ! git diff --quiet HEAD origin/main; then
        echo "error: local main differs from origin/main — pull or push first" >&2
        exit 1
    fi
    if git rev-parse "$tag" >/dev/null 2>&1; then
        echo "error: tag $tag already exists" >&2
        exit 1
    fi

    # Build the annotation body from the commit log since the previous tag.
    # goreleaser uses GitHub's changelog mode for the release body anyway,
    # but a sensible `git show $tag` is nice and shows up in `git log`.
    prev=$(git describe --tags --abbrev=0 2>/dev/null || true)
    if [[ -n "$prev" ]]; then
        body=$(printf '%s\n\nChanges since %s:\n\n%s\n' "$tag" "$prev" "$(git log --pretty='- %s' "$prev"..HEAD)")
    else
        body=$(printf '%s\n\n%s\n' "$tag" "$(git log --pretty='- %s')")
    fi

    echo "==> tagging $tag"
    echo "$body" | sed 's/^/    /'
    echo

    git tag -a "$tag" -m "$body"
    git push origin "$tag"

    echo "==> pushed. release workflow: https://github.com/widdlab/krapow/actions/workflows/release.yml"
