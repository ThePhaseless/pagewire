# Contributing

Bug reports and pull requests are welcome. For a security problem, follow `SECURITY.md` instead of opening an issue.

## Set up

Install [mise](https://mise.jdx.dev), then run `mise install` in the repository. It installs the pinned Go, Bun and lint tools from `mise.toml` and `mise.lock`. The Go build also needs the C headers for libjpeg and libpng:

```sh
sudo apt-get install -y --no-install-recommends pkg-config libjpeg-turbo8-dev libpng-dev
```

Run the tools through `mise x --` so the pinned versions apply.

## Run the tests

Go, from the repository root:

```sh
mise x -- go vet ./...
mise x -- golangci-lint run
mise x -- go test -race -count=1 ./...
mise x -- go run ./cmd/pagewire validate examples/pagewire.yaml examples/simulated.yaml
```

UI, in `ui/`:

```sh
bun install --frozen-lockfile
bun run gen && bun run typecheck && bun run lint && bun run test && bun run build
```

Browser tests run the real binary with the simulated backend. Install the browser once, then run them in `ui/`:

```sh
bunx playwright-core install chromium-headless-shell
bun run e2e
```

The integration test runs the real daemons in Docker. `test/integration/README.md` has the steps.

Kubernetes manifests and workflows:

```sh
mise x -- kustomize build deploy/base | mise x -- kubeconform -strict -summary -kubernetes-version 1.37.0 -
mise x -- kube-linter lint deploy/base internal/backend/k8s/testdata
mise x -- actionlint
```

The driver index (`internal/catalog/drivers.tsv.gz`) and the SANE index (`internal/catalog/sane-usb.tsv`) are generated from the driver and device images and committed; the dispatcher embeds them. Regenerate both after you change `images/` or `tools/driverindex`/`tools/saneindex`:

```sh
docker buildx build -f images/driver/Dockerfile --target driver-index -o type=local,dest=.cache/idx/driver-index .
docker buildx build -f images/device/Dockerfile --target sane-index -o type=local,dest=.cache/idx/sane-index .
cp .cache/idx/driver-index/drivers.tsv.gz .cache/idx/sane-index/sane-usb.tsv internal/catalog/
```

This builds every driver image and the device image, so the first run takes a while. CI's `indexes` job builds the same targets for linux/amd64 and compares the content (the gzip bytes may differ). Generate on amd64 when you can; if your arm64 build and CI disagree, use CI's files.

The images install Ubuntu packages without pinning them, so an Ubuntu update to a driver or SANE package can make the `indexes` job fail on any branch, `main` included, without a change in the repository. The job's error says so. To fix it, download the `indexes` artifact from the failed run (or run the commands above), copy `driver-index/drivers.tsv.gz` and `sane-index/sane-usb.tsv` into `internal/catalog/`, and open a pull request with both files, for example as `chore(catalog): regenerate driver and SANE indexes`. Until it merges, pull requests cannot pass `ci-ok`; images from `main` and tags still publish.

On `renovate/**` branches you do not need to do this: the Indexes workflow (`.github/workflows/indexes.yml`) regenerates both files, commits them as `github-actions[bot]` and starts CI on that commit. When Renovate rebases the branch, the rebase drops that commit, and the push starts the workflow again. CI runs on `renovate/**` branches never push images or `-sources` images.

If you change a type in `internal/config` or `internal/apitypes`, regenerate the schemas and the UI's Zod code in the same commit:

```sh
UPDATE_SCHEMA=1 mise x -- go test ./internal/schema
(cd ui && bun run gen)
```

`AGENTS.md` lists the project's rules, for example that Go code never starts processes.

## Commits and pull requests

- Write [Conventional Commits](https://www.conventionalcommits.org): `feat(config): ...`, `fix(api): ...`, `docs: ...`.
- Write tests first for every change in behaviour.
- Change `go.mod`, `go.sum`, `ui/package.json` or `ui/bun.lock` in a commit of their own.
- A pull request can merge once the `ci-ok` check passes. That one check covers every CI job.
