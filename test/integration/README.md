# Integration test

Build the three images first: `docker buildx build --load -t printscan:dev .`, `docker buildx build --load -f images/device/Dockerfile --target device -t printscan-device:dev .` and `docker buildx build --load -f images/driver/Dockerfile --target ps -t printscan-driver-ps:dev .`. The device and driver Dockerfiles end in a `sources` stage, so always pass `--target`.

Then run from the repo root: `printf %s "$(openssl rand -hex 16)" > test/integration/token && docker compose -f compose.test.yaml up -d && go test -tags integration -count=1 ./test/integration/...; docker compose -f compose.test.yaml down -v`.

Every service uses the host network, so stop any avahi-daemon on the host first (`ss -ulpn | grep 5353` must print nothing): two mDNS responders on one host share port 5353 and make browsing unreliable, so the test may miss the advertised services.

## Docker backend

`TestDockerBackend` (build tags `integration,docker`) runs the control plane from `compose.docker.yaml` with `--backend=docker`. The control plane then creates the simulated-mode device containers through the Engine API, so no printer is needed. The test prints and scans, checks the containers' hardening, restarts the control plane and expects it to adopt the running containers, removes one device container and waits for the control plane to recreate it, and changes the spec to roll the workload.

Build the two images under fully qualified names, so Podman does not look them up on docker.io: `docker buildx build --load -t localhost/printscan:dev .` and `docker buildx build --load -f images/device/Dockerfile --target device -t localhost/printscan-device:dev .`.

Then run from the repo root:

```sh
DOCKER_GID=$(stat -c %g /var/run/docker.sock) PRINTSCAN_IMAGE=localhost/printscan:dev \
  PRINTSCAN_IMAGE_OVERRIDES=device=localhost/printscan-device:dev \
  docker compose -f compose.docker.yaml up -d
go test -tags integration,docker -count=1 -timeout 20m -run '^TestDockerBackend$' ./test/integration/...
```

Clean up afterwards, and before running it again: the test creates the config with `If-None-Match: *`, so a config left in the data volume fails it. `docker compose down` leaves the device containers running, because the control plane created them:

```sh
DOCKER_GID=0 docker compose -f compose.docker.yaml down -v
docker ps -aq --filter label=dev.printscan.managed-by=printscan | xargs -r docker rm -fv
docker volume ls -q --filter label=dev.printscan.managed-by=printscan | xargs -r docker volume rm
```

The last command also removes the `printscan-blobs` volume when the backend created it. The device containers use the host network, so free port 5353 first, as above. CI runs the same test against rootful Podman too, with `DOCKER_HOST=unix:///run/podman/podman.sock` and `DOCKER_SOCKET=/run/podman/podman.sock`.
