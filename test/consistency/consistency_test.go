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

	"github.com/ThePhaseless/printscan/internal/backend/k8s"
	"github.com/ThePhaseless/printscan/internal/catalog"
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
	check("Dockerfile", "oven/bun", bunVersion)
}

var digestRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

func TestFromLinesPinned(t *testing.T) {
	for _, file := range dockerfiles {
		declared := map[string]bool{}
		for _, s := range parseDockerfile(t, file) {
			if s.image != "scratch" && !declared[s.image] && !digestRe.MatchString(s.image) {
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
		target, ok := strings.CutPrefix(p.Image, "printscan-driver-")
		if !ok {
			t.Errorf("printer %s: image %q is not printscan-driver-<target>", p.ID, p.Image)
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
	n := 0
	for _, step := range list(get(ci, "jobs", "integration", "steps")) {
		file := str(get(step, "with", "file"))
		if file == "" || !hasSourcesStage(t, file) {
			continue
		}
		n++
		if target := str(get(step, "with", "target")); target == "" || target == "sources" {
			t.Errorf("integration step %q builds %s without an explicit non-sources target", str(get(step, "name")), file)
		}
	}
	if n == 0 {
		t.Error("integration job builds no device or driver image")
	}
}

func TestCIMetaTags(t *testing.T) {
	tags := str(get(loadCI(t), "jobs", "build", "with", "meta-tags"))
	// ImageTag maps a release to its bare semver ({{version}}) and anything else to the branch tag main.
	for _, want := range []string{"type=ref,event=branch", "type=semver,pattern={{version}}"} {
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
		if strings.Contains(run, "kubeconform") && strings.Contains(run, "internal/backend/k8s/testdata/*.golden.yaml") {
			return
		}
	}
	t.Error("the go job does not run kubeconform on internal/backend/k8s/testdata/*.golden.yaml")
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

func TestE2EScript(t *testing.T) {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(read(t, "ui/package.json")), &pkg); err != nil {
		t.Fatal(err)
	}
	if e2e := pkg.Scripts["e2e"]; !strings.HasPrefix(e2e, "PRINTSCAN_E2E=1 ") {
		t.Errorf("ui/package.json e2e script %q does not start with PRINTSCAN_E2E=1", e2e)
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
