# pagewire agent guide

pagewire is a self-hosted print and scan service for one USB multifunction printer. A Go control plane (`cmd/pagewire`) serves a React UI (`ui/`), reads one YAML file and renders a device pod through the Kubernetes API (the `k8s` backend), runs the same workload as containers through the Docker Engine API (the `docker` backend, Docker or rootful Podman), or runs in-process virtual devices (the `sim` backend). `README.md` describes the product; this file holds the rules that keep the code consistent.

## Setup and commands

Once per machine: `mise install`, then the cgo headers go-mfp's image code links against:

```sh
sudo apt-get install -y --no-install-recommends pkg-config libjpeg-turbo8-dev libpng-dev
```

Without them every Go build and test fails at the cgo step. Run tools through `mise x --` so the pins in `mise.toml` apply.

| What | Command |
|---|---|
| Go vet, lint, test | `mise x -- go vet <dirs>`, `mise x -- golangci-lint run <dirs>`, `mise x -- go test -race -count=1 <dirs>` |
| Validate configs | `mise x -- go run ./cmd/pagewire validate examples/pagewire.yaml examples/simulated.yaml` |
| Run locally | `mise x -- go run ./cmd/pagewire serve --backend=sim --data-dir .cache/data --config .cache/data/pagewire.yaml` |
| UI | in `ui/`: `bun install --frozen-lockfile`, then `bun run gen`, `typecheck`, `lint`, `test`, `build`, `e2e` |
| Manifests | `mise x -- kustomize build deploy/base \| mise x -- kubeconform -strict -summary -kubernetes-version 1.37.0 -` and `mise x -- kube-linter lint deploy/base internal/backend/k8s/testdata` |
| Workflows | `mise x -- actionlint` |
| Docker backend end to end | `test/integration/README.md`, section "Docker backend" (`compose.docker.yaml`, `go test -tags integration,docker ...`) |

`.github/workflows/ci.yml` is the full list CI runs; match it before calling work done.

## Never shell out

No Go file imports `os/exec` or `golang.org/x/sys/execabs`, or calls `os.StartProcess`, `syscall.StartProcess`, `syscall`/`unix` `Exec` or `ForkExec`, or uses `SYS_EXECVE`; depguard and forbidigo in `.golangci.yml` fail the lint (test/integration included, through `run.build-tags`). A CI step also fails when a dependency beyond the known three (godbus, client-go's exec auth plugin, gnostic-models) imports `os/exec` into `./cmd/pagewire`. Talk to daemons over IPP, eSCL, D-Bus, the Kubernetes API or the Docker Engine API. Daemons are containers in the rendered pod spec (on docker, containers created from the same plan), or in-process virtual devices in `sim`. No Go test spawns a process. The browser e2e harness (`ui/e2e/harness.ts`) builds and starts the binary; it is test infrastructure, outside this rule. Check with:

```sh
grep -rn --include='*.go' --exclude-dir=node_modules --exclude-dir=.cache '"os/exec"' .
```

It prints nothing. Rendered pod specs carry no exec probes either.

## Contracts

The contracts are code, and the generated files follow them:

| Contract | Source | Generated |
|---|---|---|
| Config file `pagewire/v1` | `internal/config/config.go` | `schema/config.schema.json`, `ui/src/gen/config.ts` |
| REST DTOs | `internal/apitypes/apitypes.go` | `schema/api.schema.json`, `ui/src/gen/api.ts` |
| Device, Backend, agent protocol | `internal/device/*.go` | none |
| Printer families, driver priority, command sets, EULAs, blobs | `internal/catalog/catalog.yaml` | none |
| Driver index (every driver the printer applications list) | the driver images' `drivers` lists (`images/driver/Dockerfile`), `tools/driverindex` | `internal/catalog/drivers.tsv.gz` |
| SANE USB index (USB ID to SANE backends) | the device image's `sane.d` and backends, sane-backends `.desc` files (`images/device/Dockerfile`), `tools/saneindex` | `internal/catalog/sane-usb.tsv` |

A contract change lands as one commit holding the Go types, the regenerated schemas and the regenerated Zod source:

```sh
UPDATE_SCHEMA=1 mise x -- go test ./internal/schema
cd ui && mise x -- bun run gen
```

Dependency manifests (`go.mod`, `go.sum`, `ui/package.json`, `ui/bun.lock`) change in their own commit.

### Driver and SANE indexes

The dispatcher (`internal/catalog/dispatch.go`) has no vendor rules: it matches the device's IEEE-1284 MFG and MDL against the driver index, ranks families by `driverPriority`, falls back to `commandSets`, and takes SANE backends from the SANE index. Never hand-edit either index, and never add a per-vendor rule to `catalog.yaml`. Regenerate both whenever `images/` or `tools/` change:

```sh
docker buildx build -f images/driver/Dockerfile --target driver-index -o type=local,dest=.cache/idx/driver-index .
docker buildx build -f images/device/Dockerfile --target sane-index -o type=local,dest=.cache/idx/sane-index .
cp .cache/idx/driver-index/drivers.tsv.gz .cache/idx/sane-index/sane-usb.tsv internal/catalog/
```

- Each driver image's final stage writes its list to `/usr/share/pagewire/drivers.txt` in a `RUN` directly after its `ENV ... PPD_DIRS=...` line, so the list sees the container's environment.
- CI's `indexes` job builds both targets for linux/amd64 and diffs the decompressed content against the committed files. It is in `ci-ok.needs` and never in `build.needs`: a mismatch blocks merges but never stops `main` or a tag from publishing.
- `.github/workflows/indexes.yml` regenerates the indexes on `renovate/**` pushes, commits them as `github-actions[bot]` (listed in `renovate.json` `gitIgnoredAuthors`, so Renovate keeps rebasing), pushes without force and dispatches `ci.yml` on the branch, because pushes made with `GITHUB_TOKEN` start no workflow. Its `paths` include both index files: a Renovate rebase drops the bot commit, so the index files are what that push changes.
- ci.yml runs on `refs/heads/renovate/**` never push images or `-sources` images: `build.with.push`, `sources.if` and the `skipped)` line of `ci-ok` carry the same `startsWith(github.ref, 'refs/heads/renovate/')` condition, and `test/consistency` pins all three.
- Ubuntu package drift: the images install unpinned Ubuntu packages, so an update can change a list with no repository change, and the `indexes` job then fails on every branch. Fix it on any branch: run the commands above (or take the failed run's `indexes` artifact), commit both files in a pull request.

### JSON shapes

- Arrays in API responses and SSE events are never `null`. Every writer goes through `encodeJSON` in `internal/api/json.go`, whose `normalize` copies the value with nil slices made empty. Add new response writers through it.
- Every pointer reachable from an API DTO is either `jsonschema:"nullable"` without `omitempty` (always present, `null` when absent) or `json:",omitempty"` (optional config fields such as `device.usb`). `TestPointersNullable` in `internal/schema` enforces it.
- API DTOs hold no Go maps; `TestNoMaps` enforces it.
- GET, PUT and PATCH config, and the 412 of PUT and PATCH, all answer one `ConfigResponse` `{etag, config, state, errors, lastGood}`, built by the one `configResponse()` helper in `internal/api/config.go`.
- Status enums always hold a schema value, never `""`. Before any input `controller.Current()` returns the initial Status: `config.state: missing`, `device.phase: none`, `device.mode: none`, `printer.state` and `scanner.state: unknown`, empty lists, null `decision` and `inventory`, zero `changedAt`. `device.mode` is the current spec's mode, `none` while the spec is nil.

## Controller and backends

- **Concurrency.** The controller holds its state mutex only to read or write its own fields, never across `backend.Apply`, `backend.Observe`, `RestartPod` or printer and scanner queries. `AgentReport` never calls the backend. The `sim` backend's in-memory agent runs in its own goroutine and is the only caller of the report function; `sim.Apply` never calls it and never waits for an in-flight report. Two tests guard this (a fake backend calling back into the controller, and a mutex held around `sim.Apply`); keep them green.
- **Effective config.** `controller.Config()` is the only source of config values outside the config path: the applied config, else the last-known-good while the file is invalid, else `config.Default("Printer")`. Read it per use (the upload limit per request, scan retention per janitor run), so edits apply without a restart.
- **Printer URI, one rule for every backend.** PAPPL families: the controller takes port and path from the agent's latest `QueueResult.PrinterURI` and joins them with `Observation.HostIP` (the node IP in k8s; in docker `--host-ip`, else the default route's source address, re-read per `Observe`; 127.0.0.1 in sim). `sim` leaves `Endpoints.PrinterURI` empty for PAPPL families. `ipp-usb` and `sim` families: `Observation.Endpoints.PrinterURI` in sim, `pickIPP` over browsed `_ipp._tcp` services in k8s and docker.
- **Pod hash.** `device.Spec` holds exactly what the renderer and `sim.Apply` read, and its hash is `device.SpecHash` (nil and empty slices hash alike). Anything else stays out of `Spec`: PAPPL queue settings travel in `DesiredState.Queue` and the agent applies them on its next cycle, so they never roll the pod. `BlobRef.Optional` is a renderer input and stays in.
- **Self image.** The device pod's `probe`, `agent` and `fetch-blobs` containers run the image of the container named `pagewire` (`k8s.SelfContainer`) in the owner Deployment. Only without that container does it fall back to the registry tag or the `pagewire=` override. The name in `deploy/base/deployment.yaml` is load-bearing. On docker the self image is the image ID of the container named by `--docker-container` (`docker.DefaultContainer`, `pagewire`, pinned by `container_name` in `compose.docker.yaml`); when that container is not found it falls back the same way.
- **Docker backend.** `internal/backend/docker` translates `k8s.Plan`, so it never renders on its own; change the workload in `render.go` and both backends follow. `Apply` only records intent and changes no backend state when it fails. `Run` is the worker that pulls, probes and starts containers; it does nothing before the first successful `Apply`, so a restarted control plane never tears down a running workload. `Observe` lists containers and never waits on the worker. The workload hash (spec hash, planned containers, config files) decides between adopting what runs and rolling out. The backend only stops or removes objects labelled `io.github.thephaseless.pagewire.managed-by=pagewire`, and containers only when their names also start with `pagewire-device-`. USB on docker: ipp-usb, the printer application and AirSane get the matched printer's usbfs node alone (`withDevice`, fed by `device.USBDeviceSetter`) and are left out while it is unknown; only the probe binds all of `/dev/bus/usb`, read-only. A new `Engine` method needs its path in the allowlist of `compose.docker-proxy.yaml` and in the call list in `SECURITY.md`.
- **Job events.** The print handler watches each job it submitted until a terminal state and publishes a `job` SSE event per state or reasons change; DELETE publishes the canceled job. LAN jobs are not watched; the UI's `useJobs()` refetches on printer-state change and SSE reconnect.

## Tests

- Write the test first and watch it fail.
- Go patterns in use: table tests with inline YAML; `httptest` against fakes; `internal/pappl/papplfake` for PAPPL; `internal/blobs/blobtest` for a synthetic vendor (key, archive, signature, in-memory transport; never the network); client-go's fake clientset; an injected fake clock; sysfs trees in `t.TempDir()`; the in-memory Docker `Engine` in `internal/backend/docker/fake_test.go` (no daemon in unit tests). Tests that need an unwritable directory skip as root. A test that could hang carries a deadline and fails on it.
- Golden files: `UPDATE_SCHEMA=1` rewrites `schema/*.json`, `UPDATE_GOLDEN=1` rewrites `internal/backend/k8s/testdata/*.golden.yaml` and `internal/backend/docker/testdata/*.golden.json`. After regenerating the goldens, run `mise x -- kubeconform -strict -summary -kubernetes-version 1.37.0 internal/backend/k8s/testdata/*.golden.yaml`.
- `mise x -- go vet -tags integration,docker ./test/integration/...` compiles the Docker backend integration test, which plain `go vet ./...` skips.
- UI copy lives in `ui/src/strings.ts` (shared) and `*.strings.ts` beside each view or component; components hard-code no user-visible text.
- `ui/src/gen` is generated: Biome skips it, `tsc` still type-checks it.
- `bun test` preloads happy-dom unless `PAGEWIRE_E2E=1`; `bun run e2e` sets it. `ui/e2e/harness.ts` throws at load if a `window` global exists.
- e2e console rule: any `console` error or `pageerror` fails the scenario, CSP violations included. `consoleAllowlist` stays empty. The one built-in exclusion is Chromium's "Failed to load resource: the server responded with a status of" line for `/api/v1/` URLs; each scenario instead declares its expected 4xx and 5xx API responses with `expectApiStatus`, and any other fails. Fix a CSP violation in the file at fault; the CSP is never loosened to make a test pass.

## Images and CI

- The device and driver Dockerfiles end with a `sources` stage, so every build names its target: `--target device`, or `ps`, `hplip`, `gutenprint`, `ghostscript`, `legacy`, or the index targets `driver-index` (driver Dockerfile) and `sane-index` (device Dockerfile). CI matrix entries and the `indexes` job do too. New stages go before `lists-local`, so `sources` stays last.
- Every `FROM` naming an external image is `tag@sha256:<multi-arch index digest>` (from `docker buildx imagetools inspect`). Only `FROM scratch`, earlier stage names, `${PUSHED_IMAGE}` in the `pushed-amd64`/`pushed-arm64` stages and `lists-${SOURCES_LISTS}` go without.
- Upstream sources built in the Dockerfiles are pinned by commit: `ARG X=<40-hex sha> # renovate: datasource=github-tags depName=<owner/repo> currentValue=<tag>`. Renovate's regex manager reads that comment, so keep its shape.
- Each device and driver final stage writes `ghcr.io/thephaseless/<its image>:${VERSION#v}-sources` to `/usr/share/pagewire/SOURCES` and to the `io.github.thephaseless.pagewire.sources` label, and copies `NOTICE` and `THIRD_PARTY_NOTICES.md` beside it.
- The control-plane `Dockerfile` links cgo statically through `/usr/local/bin/pkg-config-static` (`exec pkgconf --static "$@"`) with `ENV PKG_CONFIG` pointing at it: cmd/go reads only the first word of `$PKG_CONFIG`, so `--static` cannot go into the variable.
- The `sources` CI job runs for every push (branch or tag) except on `renovate/**` branches, reads the package lists from the images the same run pushed (`SOURCES_LISTS=pushed`, `PUSHED_IMAGE=...:sha-<commit>`), and sets `meta-flavor: latest=false`; only the image build moves `latest`. `concurrency.cancel-in-progress` is true only for pull requests, so a pushing run never stops between images and their sources. `test/consistency` asserts these CI and Dockerfile rules; change the test with the rule.
- `ui/dist/.keep` stays committed so Go builds without a UI build; `bun run build` keeps it.

## Docs

`README.md`, `SECURITY.md`, `CONTRIBUTING.md` and `docs/kubernetes.md` name flags, environment variables, status codes, problem types, limits and file paths. When you change one of those in code, `git grep` for it and update the docs in the same change.

## Commits

Conventional commits (`feat(config): ...`, `fix(api): ...`). The message ends on its own content, with no `Co-Authored-By` or "Generated with" trailers.
