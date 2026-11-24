# Integration test

Build the three images first: `docker buildx build --load -t printscan:dev .`, `docker buildx build --load -f images/device/Dockerfile --target device -t printscan-device:dev .` and `docker buildx build --load -f images/driver/Dockerfile --target ps -t printscan-driver-ps:dev .`. The device and driver Dockerfiles end in a `sources` stage, so always pass `--target`.

Then run from the repo root: `printf %s "$(openssl rand -hex 16)" > test/integration/token && docker compose -f compose.test.yaml up -d && go test -tags integration -count=1 ./test/integration/...; docker compose -f compose.test.yaml down -v`.

Every service uses the host network, so stop any avahi-daemon on the host first (`ss -ulpn | grep 5353` must print nothing): two mDNS responders on one host share port 5353 and make browsing unreliable, so the test may miss the advertised services.
