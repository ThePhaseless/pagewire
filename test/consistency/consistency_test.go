// Package consistency checks facts that are written down in more than one file
// (toolchain pins, image targets, ports, CI settings) against each other.
package consistency

import (
	"encoding/json"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/ThePhaseless/pagewire/internal/backend/docker"
	"github.com/ThePhaseless/pagewire/internal/backend/k8s"
	"github.com/ThePhaseless/pagewire/internal/catalog"
)

const root = "../.."

var dockerfiles = []string{"Dockerfile", "images/device/Dockerfile", "images/driver/Dockerfile"}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS(root), rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stage is one FROM block of a Dockerfile, with its continuation lines joined.
type stage struct {
	image, name string
	lines       []string
}

func parseDockerfile(t *testing.T, rel string) []stage {
	t.Helper()
	text := strings.ReplaceAll(read(t, rel), "\\\n", " ")
	var stages []stage
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.EqualFold(f[0], "FROM") {
			args := slices.DeleteFunc(f[1:], func(s string) bool { return strings.HasPrefix(s, "--") })
			if len(args) == 0 {
				t.Fatalf("%s: FROM without an image: %q", rel, line)
			}
			s := stage{image: args[0]}
			if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
				s.name = args[2]
			}
			stages = append(stages, s)
			continue
		}
		if len(stages) > 0 {
			stages[len(stages)-1].lines = append(stages[len(stages)-1].lines, line)
		}
	}
	if len(stages) == 0 {
		t.Fatalf("%s: no FROM lines", rel)
	}
	return stages
}

func findStage(stages []stage, name string) *stage {
	for i := range stages {
		if stages[i].name == name {
			return &stages[i]
		}
	}
	return nil
}

// exposed returns the ports of a stage's EXPOSE lines.
func exposed(s *stage) []string {
	var out []string
	for _, l := range s.lines {
		f := strings.Fields(l)
		if len(f) > 0 && f[0] == "EXPOSE" {
			out = append(out, f[1:]...)
		}
	}
	return out
}

func miseTool(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`).FindStringSubmatch(read(t, "mise.toml"))
	if m == nil {
		t.Fatalf("mise.toml has no %s pin", name)
	}
	return m[1]
}

func TestToolchainPins(t *testing.T) {
	check := func(file, image, want string) {
		t.Helper()
		n := 0
		for _, s := range parseDockerfile(t, file) {
			ref, ok := strings.CutPrefix(s.image, image+":")
			if !ok {
				continue
			}
			n++
			tag, _, _ := strings.Cut(ref, "@")
			if v, _, _ := strings.Cut(tag, "-"); v != want {
				t.Errorf("%s: %s tag %q, mise.toml pins %s", file, image, tag, want)
			}
		}
		if n == 0 {
			t.Errorf("%s: no %s: stage", file, image)
		}
	}
	goVersion, bunVersion := miseTool(t, "go"), miseTool(t, "bun")
	check("Dockerfile", "golang", goVersion)
	check("images/device/Dockerfile", "golang", goVersion)
	check("images/driver/Dockerfile", "golang", goVersion)
	check("Dockerfile", "oven/bun", bunVersion)
}

var digestRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

func TestFromLinesPinned(t *testing.T) {
	for _, file := range dockerfiles {
		declared := map[string]bool{}
		for _, s := range parseDockerfile(t, file) {
			switch {
			case s.image == "${PUSHED_IMAGE}":
				// The image CI pushed for this commit, read by the sources stage (never built in local mode).
				if !strings.HasPrefix(s.name, "pushed-") {
					t.Errorf("%s: FROM ${PUSHED_IMAGE} in stage %q, allowed only in pushed-* stages", file, s.name)
				}
			case s.image == "lists-${SOURCES_LISTS}":
				if !declared["lists-local"] || !declared["lists-pushed"] {
					t.Errorf("%s: FROM lists-${SOURCES_LISTS} before both lists-local and lists-pushed", file)
				}
			case s.image != "scratch" && !declared[s.image] && !digestRe.MatchString(s.image):
				t.Errorf("%s: FROM %s is neither scratch, an earlier stage nor pinned as tag@sha256:<digest>", file, s.image)
			}
			if s.name != "" {
				declared[s.name] = true
			}
		}
	}
}

func TestStaticPkgConfigWrapper(t *testing.T) {
	text := read(t, "Dockerfile")
	if !regexp.MustCompile(`(?m)^ENV PKG_CONFIG=/usr/local/bin/pkg-config-static\s*$`).MatchString(text) {
		t.Error("Dockerfile does not set ENV PKG_CONFIG=/usr/local/bin/pkg-config-static")
	}
	found := false
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "/usr/local/bin/pkg-config-static") && strings.Contains(line, "pkgconf --static") {
			found = true
		}
	}
	if !found {
		t.Error("Dockerfile has no line writing the pkg-config-static wrapper with `pkgconf --static`")
	}
}

func TestAptPackageNames(t *testing.T) {
	for _, file := range dockerfiles {
		for _, s := range parseDockerfile(t, file) {
			for _, l := range s.lines {
				if !strings.Contains(l, "apt-get install") {
					continue
				}
				for _, w := range strings.Fields(l) {
					switch strings.Trim(w, `'"`) {
					case "sane-backends":
						t.Errorf("%s: sane-backends is a source package only; install libsane1 (and libsane-hpaio)", file)
					case "libpng16-16":
						t.Errorf("%s: libpng16-16 is virtual on Ubuntu 26.04; name libpng16-16t64", file)
					}
				}
			}
		}
	}
}

func TestHPLIPVersion(t *testing.T) {
	m := regexp.MustCompile(`hplip=([0-9][0-9.]*[0-9])`).FindStringSubmatch(read(t, "images/driver/Dockerfile"))
	if m == nil {
		t.Fatal("images/driver/Dockerfile pins no hplip= version")
	}
	pinned := m[1]
	c, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	b, ok := c.Blob("hplip-plugin")
	if !ok {
		t.Fatal("catalog has no hplip-plugin blob")
	}
	e, ok := c.EULA(b.EULA)
	if !ok {
		t.Fatalf("catalog has no EULA %s", b.EULA)
	}
	if e.Version != pinned {
		t.Errorf("catalog EULA %s version %s, driver image hplip=%s", e.ID, e.Version, pinned)
	}
	u := regexp.MustCompile(`hplip-([0-9.]+)-plugin\.run$`).FindStringSubmatch(b.URL)
	if u == nil || u[1] != pinned {
		t.Errorf("blob URL %s does not carry hplip %s", b.URL, pinned)
	}
}

func loadCI(t *testing.T) map[string]any {
	t.Helper()
	var ci map[string]any
	if err := yaml.Unmarshal([]byte(read(t, ".github/workflows/ci.yml")), &ci); err != nil {
		t.Fatal(err)
	}
	return ci
}

// get walks nested maps by key; a missing key gives nil.
func get(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func str(v any) string { s, _ := v.(string); return s }

func list(v any) []any { l, _ := v.([]any); return l }

func hasSourcesStage(t *testing.T, file string) bool {
	return findStage(parseDockerfile(t, file), "sources") != nil
}

func TestCatalogFamiliesMatchImages(t *testing.T) {
	c, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	driver := parseDockerfile(t, "images/driver/Dockerfile")
	matrix := list(get(loadCI(t), "jobs", "build", "strategy", "matrix", "include"))
	n := 0
	for _, p := range c.Printers {
		if p.Image == "" {
			continue
		}
		n++
		target, ok := strings.CutPrefix(p.Image, "pagewire-driver-")
		if !ok {
			t.Errorf("printer %s: image %q is not pagewire-driver-<target>", p.ID, p.Image)
			continue
		}
		s := findStage(driver, target)
		if s == nil {
			t.Errorf("printer %s: images/driver/Dockerfile has no target %s", p.ID, target)
		} else if ports := exposed(s); !slices.Equal(ports, []string{strconv.Itoa(p.Port)}) {
			t.Errorf("driver target %s EXPOSE %v, catalog port %d", target, ports, p.Port)
		}
		if !slices.ContainsFunc(matrix, func(e any) bool {
			return str(get(e, "image")) == p.Image && str(get(e, "file")) == "images/driver/Dockerfile" && str(get(e, "target")) == target
		}) {
			t.Errorf("printer %s: no build matrix entry {image: %s, file: images/driver/Dockerfile, target: %s}", p.ID, p.Image, target)
		}
	}
	if n == 0 {
		t.Fatal("catalog has no driver images")
	}

	device := findStage(parseDockerfile(t, "images/device/Dockerfile"), "device")
	if device == nil {
		t.Fatal("images/device/Dockerfile has no device target")
	}
	ports := exposed(device)
	sim, _ := c.Printer("sim")
	want := []int{sim.Port}
	for _, s := range c.Scanners {
		if s.ID == "airsane" {
			want = append(want, s.Port)
		}
	}
	for _, p := range want {
		if p == 0 || !slices.Contains(ports, strconv.Itoa(p)) {
			t.Errorf("device target EXPOSE %v lacks catalog port %d", ports, p)
		}
	}
}

func TestCITargets(t *testing.T) {
	ci := loadCI(t)
	matrix := list(get(ci, "jobs", "build", "strategy", "matrix", "include"))
	if len(matrix) == 0 {
		t.Fatal("build job has no matrix")
	}
	for _, e := range matrix {
		file := str(get(e, "file"))
		if target := str(get(e, "target")); hasSourcesStage(t, file) && (target == "" || target == "sources") {
			t.Errorf("build matrix entry %v: %s ends with a sources stage, so it needs an explicit non-sources target", e, file)
		}
	}
	for _, job := range []string{"integration", "docker-backend", "indexes"} {
		n := 0
		for _, step := range list(get(ci, "jobs", job, "steps")) {
			file := str(get(step, "with", "file"))
			if file == "" || !hasSourcesStage(t, file) {
				continue
			}
			n++
			if target := str(get(step, "with", "target")); target == "" || target == "sources" {
				t.Errorf("%s step %q builds %s without an explicit non-sources target", job, str(get(step, "name")), file)
			}
		}
		if n == 0 {
			t.Errorf("%s job builds no device or driver image", job)
		}
	}
}

func TestCIMetaTags(t *testing.T) {
	tags := str(get(loadCI(t), "jobs", "build", "with", "meta-tags"))
	// ImageTag maps a release to its bare semver ({{version}}) and anything else to the branch tag main.
	// type=sha,format=long gives sha-<commit>, which the sources job reads back as PUSHED_IMAGE.
	for _, want := range []string{"type=ref,event=branch", "type=semver,pattern={{version}}", "type=sha,format=long"} {
		if !strings.Contains(tags, want) {
			t.Errorf("build meta-tags lack %s", want)
		}
	}
	if got := k8s.ImageTag("v1.2.3"); got != "1.2.3" {
		t.Errorf("ImageTag(v1.2.3) = %s, metadata-action {{version}} gives 1.2.3", got)
	}
	if got := k8s.ImageTag("main"); got != "main" {
		t.Errorf("ImageTag(main) = %s, the branch tag is main", got)
	}
}

func TestSourcesJobsKeepLatest(t *testing.T) {
	jobs, _ := get(loadCI(t), "jobs").(map[string]any)
	n := 0
	for name, job := range jobs {
		if str(get(job, "with", "target")) != "sources" {
			continue
		}
		n++
		if !strings.Contains(str(get(job, "with", "meta-flavor")), "latest=false") {
			t.Errorf("job %s builds target sources without meta-flavor latest=false", name)
		}
	}
	if n == 0 {
		t.Error("no CI job builds target sources")
	}
}

func TestCIKubeconformGoldens(t *testing.T) {
	for _, step := range list(get(loadCI(t), "jobs", "go", "steps")) {
		run := str(get(step, "run"))
		if strings.Contains(run, "kubeconform") && strings.Contains(run, "internal/backend/k8s/testdata/*.golden.yaml") &&
			strings.Contains(run, "deploy/examples/*.yaml") {
			return
		}
	}
	t.Error("the go job does not run kubeconform on internal/backend/k8s/testdata/*.golden.yaml and deploy/examples/*.yaml")
}

// expr strips the ${{ }} wrapper of a GitHub Actions expression.
func expr(s string) string {
	s = strings.TrimSpace(s)
	if in, ok := strings.CutPrefix(s, "${{"); ok {
		if in, ok = strings.CutSuffix(in, "}}"); ok {
			return strings.TrimSpace(in)
		}
	}
	return s
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// A pushing run must never be cancelled between pushing images and pushing their sources,
// and sources must run for every pushed ref, even when one build leg failed.
func TestSourcesRunForEveryPush(t *testing.T) {
	ci := loadCI(t)
	if got, want := str(get(ci, "concurrency", "cancel-in-progress")), "${{ github.event_name == 'pull_request' }}"; got != want {
		t.Errorf("concurrency.cancel-in-progress = %q, want %q", got, want)
	}
	if got, want := expr(str(get(ci, "jobs", "sources", "if"))), "!cancelled() && github.event_name != 'pull_request' && "+renovateRef+" && needs.build.result != 'skipped'"; got != want {
		t.Errorf("sources if = %q, want %q", got, want)
	}
	tags := lines(str(get(ci, "jobs", "sources", "with", "meta-tags")))
	for _, want := range []string{
		"type=ref,event=branch,suffix=-sources",
		"type=semver,pattern={{version}},suffix=-sources",
		"type=semver,pattern={{major}}.{{minor}},suffix=-sources",
		"type=semver,pattern={{major}},suffix=-sources",
	} {
		if !slices.Contains(tags, want) {
			t.Errorf("sources meta-tags lack %s", want)
		}
	}
	args := lines(str(get(ci, "jobs", "sources", "with", "build-args")))
	for _, want := range []string{
		"VERSION=${{ needs.version.outputs.version }}",
		"SOURCES_LISTS=pushed",
		"PUSHED_IMAGE=" + str(get(ci, "jobs", "sources", "with", "meta-images")) + ":sha-${{ github.sha }}",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("sources build-args %q lack %s", args, want)
		}
	}
	if got, want := str(get(ci, "jobs", "sources", "with", "meta-images")), str(get(ci, "jobs", "build", "with", "meta-images")); got != want {
		t.Errorf("sources meta-images %q, build meta-images %q", got, want)
	}
	if slices.ContainsFunc(lines(str(get(ci, "jobs", "build", "with", "build-args"))), func(l string) bool {
		return strings.HasPrefix(l, "SOURCES_LISTS=") || strings.HasPrefix(l, "PUSHED_IMAGE=")
	}) {
		t.Error("build job passes SOURCES_LISTS or PUSHED_IMAGE; the image builds use the local defaults")
	}
}

// ci-ok accepts a skipped sources job only on a pull request or a renovate/** run, which push nothing.
const ciOkSkipped = `skipped) test "${{ github.event_name }}" = pull_request || test "${{ startsWith(github.ref, 'refs/heads/renovate/') }}" = true ;;`

func TestCIOkSources(t *testing.T) {
	steps := list(get(loadCI(t), "jobs", "ci-ok", "steps"))
	var run []string
	for _, s := range steps {
		run = append(run, lines(str(get(s, "run")))...)
	}
	for _, want := range []string{
		`case "${{ needs.sources.result }}" in`,
		"success) ;;",
		ciOkSkipped,
		"*) exit 1 ;;",
	} {
		if !slices.Contains(run, want) {
			t.Errorf("ci-ok run lacks %q", want)
		}
	}
	for _, l := range run {
		if strings.HasPrefix(l, "skipped)") && l != ciOkSkipped {
			t.Errorf("ci-ok accepts a skipped sources job with %q", l)
		}
	}
}

var (
	globalArgRe   = regexp.MustCompile(`(?m)^ARG ([A-Z0-9_]+)=(\S+)`)
	renovateArgRe = regexp.MustCompile(`(?m)^ARG ([A-Z0-9_]+)=(\S+)[ \t]+# renovate:.*$`)
	commitRe      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Upstreams are built by commit, so a moved tag cannot change what an image contains.
// Renovate's custom managers must still see every pin.
func TestUpstreamPinsAreCommits(t *testing.T) {
	var cfg struct {
		CustomManagers []struct {
			MatchStrings []string `json:"matchStrings"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal([]byte(read(t, "renovate.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	var managers []*regexp.Regexp
	for _, m := range cfg.CustomManagers {
		for _, s := range m.MatchStrings {
			managers = append(managers, regexp.MustCompile(s))
		}
	}
	for _, file := range []string{"images/device/Dockerfile", "images/driver/Dockerfile"} {
		text := read(t, file)
		pins := renovateArgRe.FindAllStringSubmatch(text, -1)
		if len(pins) == 0 {
			t.Errorf("%s: no ARG pins with a renovate comment", file)
		}
		for _, p := range pins {
			if !commitRe.MatchString(p[2]) {
				t.Errorf("%s: ARG %s=%s is not a 40-hex commit", file, p[1], p[2])
			}
			n := 0
			for _, re := range managers {
				if re.MatchString(p[0] + "\n") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s: %d renovate custom managers match %q, want 1", file, n, p[0])
			}
		}
		for _, a := range globalArgRe.FindAllStringSubmatch(text, -1) {
			if strings.HasSuffix(a[1], "_VERSION") || strings.HasSuffix(a[1], "_COMMIT") {
				if !slices.ContainsFunc(pins, func(p []string) bool { return p[1] == a[1] }) {
					t.Errorf("%s: ARG %s has no renovate comment", file, a[1])
				}
			}
		}
	}
}

// The sources stage reads its package lists either from the local final stages (the default)
// or, in CI, from the amd64 and arm64 images pushed for this commit.
func TestSourcesListSwitch(t *testing.T) {
	for _, file := range []string{"images/device/Dockerfile", "images/driver/Dockerfile"} {
		text := strings.ReplaceAll(read(t, file), "\\\n", " ")
		for _, want := range []string{
			`(?m)^ARG SOURCES_LISTS=local\s*$`,
			`(?m)^ARG PUSHED_IMAGE=scratch\s*$`,
			`(?m)^FROM --platform=linux/amd64 \$\{PUSHED_IMAGE\} AS pushed-amd64\s*$`,
			`(?m)^FROM --platform=linux/arm64 \$\{PUSHED_IMAGE\} AS pushed-arm64\s*$`,
			`(?m)^FROM lists-\$\{SOURCES_LISTS\} AS lists\s*$`,
			`(?m)^COPY --from=pushed-amd64 /usr/share/pagewire/source-packages.txt /lists/`,
			`(?m)^COPY --from=pushed-arm64 /usr/share/pagewire/source-packages.txt /lists/`,
			`(?m)^COPY --from=lists /lists/ /sources/lists/\s*$`,
			`sort -u /sources/lists/\*`,
			`(?m)^COPY .*\b` + regexp.QuoteMeta(file) + `\b.* /sources/build/`,
		} {
			if !regexp.MustCompile(want).MatchString(text) {
				t.Errorf("%s: no line matching %s", file, want)
			}
		}
		stages := parseDockerfile(t, file)
		for _, name := range []string{"lists-local", "lists-pushed"} {
			if s := findStage(stages, name); s == nil || s.image != "scratch" {
				t.Errorf("%s: stage %s missing or not FROM scratch", file, name)
			}
		}
	}
}

// Each device and driver image says where its corresponding source is: a SOURCES file and a
// label naming <image>:<version without v>-sources, the tag the sources job publishes.
func TestSourcesReference(t *testing.T) {
	ci := loadCI(t)
	prefix, ok := strings.CutSuffix(str(get(ci, "jobs", "build", "with", "meta-images")), "${{ matrix.image }}")
	if !ok {
		t.Fatal("build meta-images does not end with ${{ matrix.image }}")
	}
	ref := prefix + "${IMAGE}:${VERSION#v}-sources"
	n := 0
	for _, e := range list(get(ci, "jobs", "build", "strategy", "matrix", "include")) {
		file, target, image := str(get(e, "file")), str(get(e, "target")), str(get(e, "image"))
		if !hasSourcesStage(t, file) {
			continue
		}
		n++
		s := findStage(parseDockerfile(t, file), target)
		if s == nil {
			t.Errorf("%s: no stage %s", file, target)
			continue
		}
		for _, want := range []string{
			"ARG VERSION",
			"ARG IMAGE=" + image,
			"ARG SOURCES_REF=" + ref,
			"COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/",
			`LABEL io.github.thephaseless.pagewire.sources="${SOURCES_REF}"`,
		} {
			if !slices.ContainsFunc(s.lines, func(l string) bool { return strings.TrimSpace(l) == want }) {
				t.Errorf("%s stage %s: no line %q", file, target, want)
			}
		}
		if !slices.ContainsFunc(s.lines, func(l string) bool {
			return strings.HasPrefix(l, "RUN ") && strings.Contains(l, `"${SOURCES_REF}" > /usr/share/pagewire/SOURCES`)
		}) {
			t.Errorf("%s stage %s: no RUN writing \"${SOURCES_REF}\" to /usr/share/pagewire/SOURCES", file, target)
		}
	}
	if n != 6 {
		t.Errorf("%d build matrix entries with a sources stage, want 6 (device and five drivers)", n)
	}
}

// renovateRef keeps ci.yml runs on renovate/** branches (dispatched by indexes.yml) from publishing.
const renovateRef = "!startsWith(github.ref, 'refs/heads/renovate/')"

// Runs on renovate/** build and test everything but push no images and no sources.
func TestRenovateRunsDoNotPush(t *testing.T) {
	ci := loadCI(t)
	if got, want := expr(str(get(ci, "jobs", "build", "with", "push"))), "github.event_name != 'pull_request' && "+renovateRef; got != want {
		t.Errorf("build push = %q, want %q", got, want)
	}
	if got, want := expr(str(get(ci, "jobs", "sources", "if"))), "!cancelled() && github.event_name != 'pull_request' && "+renovateRef+" && needs.build.result != 'skipped'"; got != want {
		t.Errorf("sources if = %q, want %q", got, want)
	}
	// indexes.yml dispatches ci.yml; the key's value is null, so check the key.
	if on, _ := get(ci, "on").(map[string]any); on == nil {
		t.Error("ci.yml has no on map")
	} else if _, ok := on["workflow_dispatch"]; !ok {
		t.Error("ci.yml on lacks workflow_dispatch, which indexes.yml uses to start CI on its commit")
	}
}

func stepsBuilding(steps []any, file, target string) int {
	n := 0
	for _, s := range steps {
		if str(get(s, "with", "file")) == file && str(get(s, "with", "target")) == target {
			n++
		}
	}
	return n
}

// The driver and SANE indexes are generated from the images: CI checks them, and indexes.yml
// regenerates them on Renovate branches.
func TestIndexJob(t *testing.T) {
	ci := loadCI(t)
	steps := list(get(ci, "jobs", "indexes", "steps"))
	if stepsBuilding(steps, "images/driver/Dockerfile", "driver-index") != 1 || stepsBuilding(steps, "images/device/Dockerfile", "sane-index") != 1 {
		t.Error("ci.yml indexes job does not build driver-index from images/driver/Dockerfile and sane-index from images/device/Dockerfile")
	}
	// Drift from an Ubuntu package update blocks merges, never image publishing.
	if !slices.Contains(list(get(ci, "jobs", "ci-ok", "needs")), any("indexes")) {
		t.Error("ci-ok needs lacks indexes")
	}
	if slices.Contains(list(get(ci, "jobs", "build", "needs")), any("indexes")) {
		t.Error("build needs indexes; drift must not stop main or a tag from publishing")
	}
	var okRun []string
	for _, s := range list(get(ci, "jobs", "ci-ok", "steps")) {
		okRun = append(okRun, lines(str(get(s, "run")))...)
	}
	if !slices.Contains(okRun, `test "${{ needs.indexes.result }}" = success`) {
		t.Error("ci-ok does not test needs.indexes.result")
	}
	// Caches are scoped by ref: only main writes, every other ref restores main's.
	n := 0
	for _, s := range steps {
		with, _ := get(s, "with").(map[string]any)
		if to, ok := with["cache-to"]; ok {
			n++
			if !strings.Contains(str(to), "refs/heads/main") {
				t.Errorf("indexes step %q writes its cache from any ref: %q", str(get(s, "name")), str(to))
			}
		}
	}
	if n == 0 {
		t.Error("indexes job has no cache-to")
	}

	for file, target := range map[string]string{"images/driver/Dockerfile": "driver-index", "images/device/Dockerfile": "sane-index"} {
		stages := parseDockerfile(t, file)
		if findStage(stages, target) == nil {
			t.Errorf("%s: no stage %s", file, target)
		}
		if last := stages[len(stages)-1].name; last != "sources" {
			t.Errorf("%s: last stage %q, want sources (the default target)", file, last)
		}
	}

	// Each driver image dumps its list with the container's PPD_DIRS, so the dump follows that ENV line.
	driver := parseDockerfile(t, "images/driver/Dockerfile")
	targets := 0
	for _, e := range list(get(ci, "jobs", "build", "strategy", "matrix", "include")) {
		if str(get(e, "file")) != "images/driver/Dockerfile" {
			continue
		}
		targets++
		target := str(get(e, "target"))
		s := findStage(driver, target)
		if s == nil {
			t.Errorf("images/driver/Dockerfile: no stage %s", target)
			continue
		}
		env := slices.IndexFunc(s.lines, func(l string) bool {
			l = strings.TrimSpace(l)
			return strings.HasPrefix(l, "ENV ") && strings.Contains(l, "PPD_DIRS=")
		})
		dump := slices.IndexFunc(s.lines, func(l string) bool {
			l = strings.TrimSpace(l)
			return strings.HasPrefix(l, "RUN ") && strings.Contains(l, "drivers > /usr/share/pagewire/drivers.txt")
		})
		switch {
		case env < 0 || dump < 0:
			t.Errorf("images/driver/Dockerfile stage %s: needs an ENV ... PPD_DIRS= line and a RUN dumping drivers to /usr/share/pagewire/drivers.txt", target)
		case dump < env:
			t.Errorf("images/driver/Dockerfile stage %s: the drivers.txt dump runs before the ENV ... PPD_DIRS= line", target)
		}
	}
	if targets != 5 {
		t.Errorf("%d driver build matrix entries, want 5", targets)
	}

	var wf map[string]any
	if err := yaml.Unmarshal([]byte(read(t, ".github/workflows/indexes.yml")), &wf); err != nil {
		t.Fatal(err)
	}
	branches := list(get(wf, "on", "push", "branches"))
	if !slices.Contains(branches, any("renovate/**")) {
		t.Errorf("indexes.yml push branches %v lack renovate/**", branches)
	}
	// The index paths catch Renovate rebases, which drop the regeneration commit. The generators use
	// only the standard library, so go.mod and go.sum cannot change an index.
	paths := list(get(wf, "on", "push", "paths"))
	for _, want := range []string{"images/**", "tools/**", "internal/catalog/drivers.tsv.gz", "internal/catalog/sane-usb.tsv"} {
		if !slices.Contains(paths, any(want)) {
			t.Errorf("indexes.yml push paths lack %s", want)
		}
	}
	for _, bad := range []string{"go.mod", "go.sum"} {
		if slices.Contains(paths, any(bad)) {
			t.Errorf("indexes.yml push paths list %s", bad)
		}
	}
	if on, _ := get(wf, "on").(map[string]any); on == nil {
		t.Error("indexes.yml has no on map")
	} else if _, ok := on["workflow_dispatch"]; !ok {
		t.Error("indexes.yml on lacks workflow_dispatch")
	}
	if p, _ := get(wf, "permissions").(map[string]any); len(p) != 1 || str(p["contents"]) != "read" {
		t.Errorf("indexes.yml workflow permissions %v, want contents: read", p)
	}
	jobs, _ := get(wf, "jobs").(map[string]any)
	if len(jobs) != 1 {
		t.Fatalf("indexes.yml has %d jobs, want 1", len(jobs))
	}
	for name, job := range jobs {
		if p, _ := get(job, "permissions").(map[string]any); len(p) != 2 || str(p["contents"]) != "write" || str(p["actions"]) != "write" {
			t.Errorf("indexes.yml job %s permissions %v, want exactly contents: write and actions: write", name, p)
		}
		if !strings.Contains(str(get(job, "if")), "refs/heads/renovate/") {
			t.Errorf("indexes.yml job %s if %q does not limit it to renovate/** branches", name, str(get(job, "if")))
		}
		var cfg struct {
			GitIgnoredAuthors []string `json:"gitIgnoredAuthors"`
		}
		if err := json.Unmarshal([]byte(read(t, "renovate.json")), &cfg); err != nil {
			t.Fatal(err)
		}
		dispatch := false
		for _, s := range list(get(job, "steps")) {
			run := str(get(s, "run"))
			for _, l := range lines(run) {
				if strings.Contains(l, "git push") && (strings.Contains(l, "--force") || strings.Contains(l+" ", " -f ")) {
					t.Errorf("indexes.yml force-pushes: %q", l)
				}
			}
			if !strings.Contains(run, "gh workflow run ci.yml --ref") {
				continue
			}
			dispatch = true
			// Renovate keeps rebasing a branch whose extra commits come from ignored authors.
			m := regexp.MustCompile(`user\.email\s+['"]?([^'"\s]+)`).FindStringSubmatch(run)
			if m == nil {
				t.Error("indexes.yml commit step sets no git user.email")
			} else if !slices.Contains(cfg.GitIgnoredAuthors, m[1]) {
				t.Errorf("indexes.yml commits as %s, which renovate.json gitIgnoredAuthors %v lacks", m[1], cfg.GitIgnoredAuthors)
			}
		}
		if !dispatch {
			t.Error("indexes.yml has no step running gh workflow run ci.yml --ref")
		}
	}
}

func TestDeploymentContainerName(t *testing.T) {
	var d map[string]any
	if err := yaml.Unmarshal([]byte(read(t, "deploy/base/deployment.yaml")), &d); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(list(get(d, "spec", "template", "spec", "containers")), func(c any) bool {
		return str(get(c, "name")) == k8s.SelfContainer
	}) {
		t.Errorf("deploy/base/deployment.yaml has no container named %q (k8s.SelfContainer)", k8s.SelfContainer)
	}
}

// The device containers run the image of the container named docker.DefaultContainer,
// and the stack's Compose project must not be the demo's or the integration test's.
func TestComposeDockerContainerName(t *testing.T) {
	var c, it map[string]any
	if err := yaml.Unmarshal([]byte(read(t, "compose.docker.yaml")), &c); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(read(t, "compose.test.yaml")), &it); err != nil {
		t.Fatal(err)
	}
	if got := str(get(c, "services", "pagewire", "container_name")); got != docker.DefaultContainer {
		t.Errorf("compose.docker.yaml container_name = %q, want %q (docker.DefaultContainer)", got, docker.DefaultContainer)
	}
	if !slices.ContainsFunc(list(get(c, "services", "pagewire", "command")), func(a any) bool { return str(a) == "--backend=docker" }) {
		t.Error("compose.docker.yaml command lacks --backend=docker")
	}
	// compose.yaml has no name, so its project is the checkout directory, usually pagewire.
	if name := str(get(c, "name")); name == "" || name == "pagewire" || name == str(get(it, "name")) {
		t.Errorf("compose.docker.yaml name = %q; want a project apart from compose.test.yaml (%q) and the demo stack (pagewire)", name, str(get(it, "name")))
	}
}

func TestE2EScript(t *testing.T) {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(read(t, "ui/package.json")), &pkg); err != nil {
		t.Fatal(err)
	}
	if e2e := pkg.Scripts["e2e"]; !strings.HasPrefix(e2e, "PAGEWIRE_E2E=1 ") {
		t.Errorf("ui/package.json e2e script %q does not start with PAGEWIRE_E2E=1", e2e)
	}
}

func TestConsoleAllowlistEmpty(t *testing.T) {
	text := read(t, "ui/e2e/harness.ts")
	decl := regexp.MustCompile(`(?m)^\s*(export\s+)?const\s+consoleAllowlist\b[^=]*=\s*(.*)$`).FindAllStringSubmatch(text, -1)
	if len(decl) != 1 {
		t.Fatalf("ui/e2e/harness.ts declares consoleAllowlist %d times, want 1", len(decl))
	}
	if v := strings.TrimSpace(decl[0][2]); v != "[];" && v != "[]" {
		t.Errorf("consoleAllowlist = %s, want []", v)
	}
	if regexp.MustCompile(`consoleAllowlist\s*\.\s*(push|unshift|splice)\b`).MatchString(text) {
		t.Error("ui/e2e/harness.ts adds entries to consoleAllowlist")
	}
}
