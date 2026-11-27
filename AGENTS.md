# pagewire agent guide

pagewire is a self-hosted print and scan service for USB printers and scanners, any number of each. A Go control plane (`cmd/pagewire`) serves a React UI (`ui/`), reads one YAML file and renders one device pod per node with USB devices through the Kubernetes API (the `k8s` backend), runs the same workload as containers through the Docker Engine API (the `docker` backend, Docker or rootful Podman), or runs in-process virtual devices (the `sim` backend). `README.md` describes the product; this file holds the rules that keep the code consistent.

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
| Config file `pagewire/v2` (a `pagewire/v1` file migrates through `internal/config/migrate.go`) | `internal/config/config.go` | `schema/config.schema.json`, `ui/src/gen/config.ts` |
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
- Every pointer reachable from an API DTO is either `jsonschema:"nullable"` without `omitempty` (always present, `null` when absent) or `json:",omitempty"` (optional config fields such as `printers[].usb`). `TestPointersNullable` in `internal/schema` enforces it.
- API DTOs hold no Go maps; `TestNoMaps` enforces it.
- GET, PUT and PATCH config, and the 412 of PUT and PATCH, all answer one `ConfigResponse` `{etag, config, state, errors, lastGood}`, built by the one `configResponse()` helper in `internal/api/config.go`.
- Status enums always hold a schema value, never `""`. Before any input `controller.Current()` returns the initial Status: `config.state: missing`, `phase: none`, and empty `printers`, `scanners`, `workloads`, `devices` and `reconcile` lists. Each `StatusPrinter` and `StatusScanner` holds `state: unknown` until its endpoint answers, and each `StatusWorkload.mode` is its spec's mode, `none` while it has no spec.

## Controller and backends

- **Concurrency.** The controller holds its state mutex only to read or write its own fields, never across a backend call (`USBNodes`, `Observe`, `Apply(specs)`, `SetUSBDevices`, `RestartPod(workload)`) or printer and scanner queries. Each step lists the nodes before `Observe`, so the k8s backend's slot map is settled before it names workloads. `AgentReport` never calls the backend; a report with `Wait` may be held up to `AgentWait` (a long-poll until the desired state changes), with `mu` released while it waits. The `sim` backend's in-memory agent runs in its own goroutine and is the only caller of the report function; `sim.Apply` never calls it and never waits for an in-flight report. Two tests guard this (a fake backend calling back into the controller, and a mutex held around `sim.Apply`); keep them green.
- **Effective config.** `controller.Config()` is the only source of config values outside the config path: the applied config, else the last-known-good while the file is invalid, else `config.Default("Printer")`. Read it per use (the upload limit per request, scan retention per janitor run), so edits apply without a restart.
- **Printer URI, one rule per printer for every backend** (`printerEndpoint` in `internal/controller/status.go`). PAPPL families: the controller takes port and path from the agent's latest `QueueResult.PrinterURI` for that printer's id and joins them with `Observation.HostIP` (the node IP in k8s; in docker `--host-ip`, else the default route's source address, re-read per `Observe`; 127.0.0.1 in sim). `sim` leaves `Endpoints.PrinterURI` empty for PAPPL families. `ipp-usb` and `sim` families: the `Observation.Endpoints` entry with the printer's id in sim; in k8s and docker, `pick` over the workload's browsed `_ipp._tcp` services: on ipp-usb ports the one whose TXT `usb_MFG` and `usb_MDL` match the device's IEEE-1284 ID, ties to name order. Scanners follow the same pattern (`scannerEndpoint`).
- **Pod hash.** `device.Spec` holds exactly what the renderer and `sim.Apply` read, and its hash is `device.SpecHash` (nil and empty slices hash alike). Anything else stays out of `Spec`: PAPPL queue settings travel in `DesiredState.Queues` and the agent applies them as soon as the desired state changes, so they never roll the pod. `BlobRef.Optional` is a renderer input and stays in.
- **PAPPL queues.** The agent keeps one `pappl.Client` per resolved system URL (`QueueSpec.SystemURI`, through `Core.SystemURL` in sim) for its whole life, because the client remembers each queue's last Find-Devices pick. The agent reconciles only on an event: its first cycle, an exported `Core.Wake`, a desired-state change, a PAPPL system event (or a new subscription; without subscriptions, each heartbeat), a changed attached-device list (a USB event alone reconciles nothing; a replug changes the address), or a retry timer after an error or a pending queue whose device is present. `pappl.Reconcile` asks Find-Devices only for queues that are not settled, at most once per call for all of them, and not again within `findBackoff` (30 s) for a queue the application listed nothing for: every Find-Devices makes the application claim each printer's USB interface, which competes with a job that is printing. A USB add calls `Client.DeviceChanged`, which clears that backoff once; settled picks stay. It sends Set-Printer-Attributes only when the location or a default PAPPL can set (`ippcaps.PAPPLSettable`) differs from the queue's current one. A default the queue refuses, or one it accepted but reads back as another value (sent-but-not-read-back memory, so a Set does not loop through its own `printer-config-changed` event), is not resent until the wanted value or the queue changes; the agent reports it in `QueueResult.Refused` by option kind, and the controller turns it into a structured printer warning. Tests pin both counts (`FindDevicesCalls`, `SetCalls` on papplfake); keep them.
- **Kubernetes workloads.** One device workload per node with USB devices, in fixed slots `pagewire-device` and `pagewire-device-1` to `-3` (`k8s.MaxSlots`), because RBAC `resourceNames` match exactly. Multi-node mode needs `list` on nodes (the optional `deploy/base/clusterrole.yaml`); when the list is refused (403 or 404, `device.ErrSingleNode`) the backend runs slot 0 alone, unpinned, and, apart from `Notify`'s pods watch, makes only the calls `deploy/base/role.yaml` allowed before slots existed. Any other list error is transient: the controller keeps its last workload set and applies nothing that step. A failed slot comes back as `device.WorkloadErrors`, which fails only that workload's item and devices. `role.yaml` also grants `watch` on pods and the ClusterRole `watch` on nodes, used by `Notify`; with an older Role or ClusterRole the watch is refused, and the backend falls back to a 30 s re-list with a failed `backend-events` reconcile item. `policy_test.go` checks every call, `Notify` included in both modes, against `role.yaml` plus `clusterrole.yaml`, and the single-node call log (without `Notify`) against the pre-slot Role embedded in the test.
- **Self image.** The device pod's `probe`, `agent` and `fetch-blobs` containers run the image of the container named `pagewire` (`k8s.SelfContainer`) in the owner Deployment. Only without that container does it fall back to the registry tag or the `pagewire=` override. The name in `deploy/base/deployment.yaml` is load-bearing. On docker the self image is the image ID of the container named by `--docker-container` (`docker.DefaultContainer`, `pagewire`, pinned by `container_name` in `compose.docker.yaml`); when that container is not found it falls back the same way.
- **Docker backend.** `internal/backend/docker` translates `k8s.Plan`, so it never renders on its own; change the workload in `render.go` and both backends follow. `Apply` only records intent and changes no backend state when it fails. `Run` is the worker that pulls, probes and starts containers; it does nothing before the first successful `Apply`, so a restarted control plane never tears down a running workload. `Observe` lists containers and never waits on the worker. The workload hash (spec hash, planned containers, config files) decides between adopting what runs and rolling out. The backend only stops or removes objects labelled `io.github.thephaseless.pagewire.managed-by=pagewire`, and containers only when their names also start with `pagewire-device-`. USB on docker: each USB daemon (ipp-usb, each printer application, AirSane) gets the usbfs nodes of the devices it serves alone (`withDevice`, fed by `device.USBDeviceSetter.SetUSBDevices`) and is left out while none of them is attached; only the probe binds all of `/dev/bus/usb`, read-only. `Notify` wakes the worker (except while it backs off after a failed rollout: then only a new spec or devices, `RestartPod` or a USB uevent cut the backoff short) and the controller from the Engine's `Events` stream, filtered on the server by `type=container`, the managed-by `label` and the lifecycle `event` actions (`lifecycleActions`), with the same action filter repeated on the client and `health_status` passed only when a container's status changes (Podman logs one per healthcheck run); while the stream is unavailable `Run` falls back to `Resync`. A stream ends a failure streak (backoff reset, `backend-events` item cleared, one Info log) only after it stayed up `eventsStable` (30 s), so a proxy that keeps closing it degrades and warns once. Known limits: a proxy that closes the stream only after 30 s or more still logs a Warn and Info pair and flips the `backend-events` item every cycle, and after one drop the degraded text lingers up to 30 s; accepted, since this backend is scheduled for removal. A new `Engine` method, like `Events` (`GET /events`), needs its path in the allowlist of `compose.docker-proxy.yaml` and in the call list in `SECURITY.md`.
- **Job events.** The print handler watches each job it submitted until a terminal state, through IPP job notifications (`JobWatcher`) and a `JobPollInterval` poll when the printer has none, and publishes a `job` SSE event per state or reasons change; DELETE publishes the canceled job. After each job event `Deps.OnJob` refreshes the controller, so a push-less printer's status follows a pagewire job. LAN jobs are not watched; the UI's `useJobs(printerId)` refetches on that printer's state change and on SSE reconnect.
- **Endpoint watchers.** Under `Run`, `syncWatchers` keeps one watcher per printer and scanner URI whose device is plugged in and hands it each step's query result as its baseline, so the baseline always equals what Status shows. Every watcher polls its endpoint every `endpointPoll` (60 s) and pokes the loop only when the answer differs from the baseline. While the baseline or its own query is a failure it retries after `pollRetry` (1 s), doubling to 60 s; a step whose query fails where the last one answered restarts that retry. A printer with IPP notifications is also subscribed (`PrinterWatcher`) so its changes show at once, and keeps its poll, because PAPPL refreshes reasons and supplies only inside Get-Printer-Attributes and a hung application sends no event. A printer served by ipp-usb is polled only: a held Get-Notifications would keep one of the device's few IPP-USB interfaces busy.

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
- Each RUN with `apt-get update`, `install` or `upgrade` in a stage a device or driver image is built from appends `date -u +%Y%m%dT%H%M%SZ` to `/usr/share/pagewire/apt-snapshots.txt` before its first and after its last apt-get, inside that same RUN: with the CI layer cache, a later RUN can be days younger than the apt layer. Ubuntu source packages are downloaded only by `images/apt-source-fetch.sh`, which `sources-fetch` and the device `sane-desc` stage COPY and run with those files; it fetches a version Ubuntu has since superseded through `apt-get --snapshot` at those times. `TestAptSnapshotsRecorded` and `TestAptSourceFetchShared` pin it.
- **Fork gate.** libcupsfilters and libppd run filter functions in a child forked without exec from the multi-threaded printer application (pappl-retrofit issue 35); a lock another thread holds at fork stays locked in the child and the job hangs in processing. `images/driver/patches/pappl-retrofit-fork-gate.patch` links a fork gate into libpappl-retrofit: it interposes the pthread mutex, rwlock, condition-wait and `pthread_create` calls and glibc's time and syslog functions for every library, and `fork()` waits in a `pthread_atfork` prepare handler until no other thread holds one. Every wait in it is a futex wait on a word the other side changes, with one 5 s failsafe deadline, after which it forks anyway and logs `fork-gate: ... forking anyway`; a forked child that would block on a lock held at fork logs `fork-gate: ... exiting` and exits 70, so the job fails instead of hanging. Keep every wait event-driven, with that failsafe as its only deadline. `pappl-base` builds `images/driver/fork-gate-test.c` with and without the gate and runs every case, so CI's native amd64 and arm64 build legs both test it; the `wake`, `wake-rw`, `held` and `held-rw` cases must still hang without the gate, or they test nothing. Each driver final stage runs `check-fork-gate.sh` on its application, which fails when a library resolves a gated call to libc (link order). `TestForkGate` pins both. Drop the patch, the test and both Dockerfile steps together once issue 35 is fixed.
- **Filter chain.** pappl-retrofit 1.0b3 ran each job's `cfFilterChain()` in the job thread, and its `wait()` reaps any child, so two jobs printing at once in one application reaped each other's filters and hung (pappl-retrofit issue 36). `images/driver/patches/pappl-retrofit-filter-reap.patch` runs every chain in a child process of its own, and `_prFilterChain()` waits for that one PID. That child is a child subreaper and waits until every process the chain left behind has ended, such as the CUPS backend, because the application is PID 1 in its container and never reaps orphans. The patch also serializes the walk over the shared spooling-conversion list, so two jobs never miss their pre-filter. In the application process, every wait names its own child's PID; a `wait()` or `waitpid(-1, ...)` there steals other jobs' children, and nothing there reaps leftovers. Processes orphaned outside a chain therefore stay zombies for the life of the container: a backend left by a device listing that `_prCUPSDevList()` kills after its 15 s timeout. shortcut: no init process reaps them; run the application under one (`tini` as PID 1) once such zombies pile up. `pappl-base` builds `images/driver/filter-chain-test.c` against the patched `_prFilterChain()` and against `cfFilterChain()` and runs every case; the `steal` and `reap` cases must still fail without the patch, or they test nothing. `TestFilterChainPatch` pins it. Drop the patch, the test and its Dockerfile step together once issue 36 is fixed.
- `ui/dist/.keep` stays committed so Go builds without a UI build; `bun run build` keeps it.

## Docs

`README.md`, `SECURITY.md`, `CONTRIBUTING.md` and `docs/kubernetes.md` name flags, environment variables, status codes, problem types, limits and file paths. When you change one of those in code, `git grep` for it and update the docs in the same change.

## Commits

Conventional commits (`feat(config): ...`, `fix(api): ...`). The message ends on its own content, with no `Co-Authored-By` or "Generated with" trailers.
