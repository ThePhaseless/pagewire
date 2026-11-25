package docker

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// fakeEngine is an in-memory Engine. Containers are addressed by name or ID.
type fakeEngine struct {
	mu         sync.Mutex
	nextID     int
	containers map[string]*fakeContainer // by name
	volumes    map[string]*fakeVolume
	images     map[string]bool
	calls      []string          // "Method name"
	created    map[string]string // container name → image, kept after removal

	exitCodes map[string]int               // run-once container name → exit code
	health    map[string]*container.Health // container name → health after start; a nil value means none
	errs      map[string]error             // "Method name" → error
	pullHook  func(ctx context.Context, ref string) error
}

type fakeContainer struct {
	id, name string
	opts     client.ContainerCreateOptions
	state    container.ContainerState
	exitCode int
	health   *container.Health
	anon     []string
}

type fakeVolume struct {
	labels     map[string]string
	driverOpts map[string]string
	anon       bool
	files      map[string]string
}

func newFake() *fakeEngine {
	return &fakeEngine{
		containers: map[string]*fakeContainer{},
		volumes:    map[string]*fakeVolume{},
		images:     map[string]bool{},
		created:    map[string]string{},
		exitCodes:  map[string]int{},
		health:     map[string]*container.Health{},
		errs:       map[string]error{},
	}
}

func notFound(kind, name string) error {
	return fmt.Errorf("no such %s: %s: %w", kind, name, cerrdefs.ErrNotFound)
}

// record logs the call and returns its injected error, if any. Callers hold f.mu.
func (f *fakeEngine) record(method, name string) error {
	f.calls = append(f.calls, method+" "+name)
	return f.errs[method+" "+name]
}

func (f *fakeEngine) find(id string) *fakeContainer {
	if c, ok := f.containers[id]; ok {
		return c
	}
	for _, c := range f.containers {
		if c.id == id {
			return c
		}
	}
	return nil
}

// addContainer adds a container directly, as if created and started earlier.
func (f *fakeEngine) addContainer(name, img string, labels map[string]string, state container.ContainerState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.containers[name] = &fakeContainer{
		id: fmt.Sprintf("id%d", f.nextID), name: name, state: state,
		opts: client.ContainerCreateOptions{Name: name, Config: &container.Config{Image: img, Labels: labels}, HostConfig: &container.HostConfig{}},
	}
}

func (f *fakeEngine) setState(name string, state container.ContainerState, exit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[name].state, f.containers[name].exitCode = state, exit
}

func (f *fakeEngine) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeEngine) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeEngine) setErr(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, key)
	} else {
		f.errs[key] = err
	}
}

func (f *fakeEngine) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.containers))
}

func (f *fakeEngine) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ContainerList", ""); err != nil {
		return client.ContainerListResult{}, err
	}
	var out []container.Summary
	for _, name := range slices.Sorted(maps.Keys(f.containers)) {
		c := f.containers[name]
		match := true
		for sel := range o.Filters["label"] {
			k, v, _ := strings.Cut(sel, "=")
			if c.opts.Config.Labels[k] != v {
				match = false
			}
		}
		if match {
			out = append(out, container.Summary{
				ID: c.id, Names: []string{"/" + c.name}, Image: c.opts.Config.Image,
				Labels: maps.Clone(c.opts.Config.Labels), State: c.state,
			})
		}
	}
	return client.ContainerListResult{Items: out}, nil
}

func (f *fakeEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ContainerInspect", id); err != nil {
		return client.ContainerInspectResult{}, err
	}
	c := f.find(id)
	if c == nil {
		return client.ContainerInspectResult{}, notFound("container", id)
	}
	var h *container.Health
	if c.health != nil {
		cp := *c.health
		h = &cp
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID: c.id, Name: "/" + c.name, Image: c.opts.Config.Image,
		State: &container.State{
			Status: c.state, Running: c.state == container.StateRunning, ExitCode: c.exitCode, Health: h,
		},
		Config: c.opts.Config, HostConfig: c.opts.HostConfig,
	}}, nil
}

func (f *fakeEngine) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ContainerCreate", o.Name); err != nil {
		return client.ContainerCreateResult{}, err
	}
	if _, ok := f.containers[o.Name]; ok {
		return client.ContainerCreateResult{}, fmt.Errorf("name %s in use: %w", o.Name, cerrdefs.ErrConflict)
	}
	f.nextID++
	c := &fakeContainer{id: fmt.Sprintf("id%d", f.nextID), name: o.Name, opts: o, state: container.StateCreated}
	for i, m := range o.HostConfig.Mounts {
		if m.Type != mount.TypeVolume {
			continue
		}
		if m.Source == "" {
			name := fmt.Sprintf("anon-%s-%d", c.id, i)
			var labels map[string]string
			if m.VolumeOptions != nil {
				labels = maps.Clone(m.VolumeOptions.Labels)
			}
			f.volumes[name] = &fakeVolume{labels: labels, anon: true}
			c.anon = append(c.anon, name)
		} else if f.volumes[m.Source] == nil {
			f.volumes[m.Source] = &fakeVolume{} // the engine creates a missing named volume
		}
	}
	f.containers[o.Name] = c
	f.created[o.Name] = o.Config.Image
	return client.ContainerCreateResult{ID: c.id}, nil
}

func (f *fakeEngine) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ContainerStart", id); err != nil {
		return client.ContainerStartResult{}, err
	}
	c := f.find(id)
	if c == nil {
		return client.ContainerStartResult{}, notFound("container", id)
	}
	c.state = container.StateRunning
	if c.opts.Config.Labels[LabelInit] == "true" {
		c.state, c.exitCode = container.StateExited, f.exitCodes[c.name]
	}
	if hc := c.opts.Config.Healthcheck; hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE" {
		h, set := f.health[c.name]
		if !set {
			h = &container.Health{Status: container.Healthy, Log: []*container.HealthcheckResult{{ExitCode: 0}}}
		}
		c.health = h
	}
	return client.ContainerStartResult{}, nil
}

func (f *fakeEngine) ContainerStop(_ context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ContainerStop", f.nameOf(id)); err != nil {
		return client.ContainerStopResult{}, err
	}
	c := f.find(id)
	if c == nil {
		return client.ContainerStopResult{}, notFound("container", id)
	}
	if c.state == container.StateRunning || c.state == container.StateRestarting {
		c.state = container.StateExited
	}
	return client.ContainerStopResult{}, nil
}

func (f *fakeEngine) nameOf(id string) string {
	if c := f.find(id); c != nil {
		return c.name
	}
	return id
}

func (f *fakeEngine) ContainerRemove(_ context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ContainerRemove", f.nameOf(id)); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	c := f.find(id)
	if c == nil {
		return client.ContainerRemoveResult{}, notFound("container", id)
	}
	if c.state == container.StateRunning && !o.Force {
		return client.ContainerRemoveResult{}, fmt.Errorf("%s is running: %w", c.name, cerrdefs.ErrConflict)
	}
	if o.RemoveVolumes {
		for _, v := range c.anon {
			delete(f.volumes, v)
		}
	}
	delete(f.containers, c.name)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeEngine) ContainerWait(ctx context.Context, id string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := make(chan container.WaitResponse, 1)
	errc := make(chan error, 1)
	err := f.record("ContainerWait", id)
	c := f.find(id)
	switch {
	case err != nil:
		errc <- err
	case c == nil:
		errc <- notFound("container", id)
	case c.state == container.StateExited:
		res <- container.WaitResponse{StatusCode: int64(c.exitCode)}
	default:
		go func() { <-ctx.Done(); errc <- ctx.Err() }()
	}
	return client.ContainerWaitResult{Result: res, Error: errc}
}

func (f *fakeEngine) CopyToContainer(_ context.Context, id string, o client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CopyToContainer", id+":"+o.DestinationPath); err != nil {
		return client.CopyToContainerResult{}, err
	}
	c := f.find(id)
	if c == nil {
		return client.CopyToContainerResult{}, notFound("container", id)
	}
	var vol *fakeVolume
	for _, m := range c.opts.HostConfig.Mounts {
		if m.Target == o.DestinationPath && m.Source != "" && !m.ReadOnly {
			vol = f.volumes[m.Source]
		}
	}
	if vol == nil {
		return client.CopyToContainerResult{}, errors.New("container rootfs is marked read-only")
	}
	if vol.files == nil {
		vol.files = map[string]string{}
	}
	tr := tar.NewReader(o.Content)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return client.CopyToContainerResult{}, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return client.CopyToContainerResult{}, err
		}
		vol.files[h.Name] = string(b)
	}
	return client.CopyToContainerResult{}, nil
}

func (f *fakeEngine) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ImageInspect", ref); err != nil {
		return client.ImageInspectResult{}, err
	}
	if !f.images[ref] {
		return client.ImageInspectResult{}, notFound("image", ref)
	}
	return client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: "sha256:" + ref}}, nil
}

type fakePull struct {
	io.Reader
	f   *fakeEngine
	ref string
}

func (p *fakePull) Close() error { return nil }

func (p *fakePull) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}

func (p *fakePull) Wait(ctx context.Context) error {
	p.f.mu.Lock()
	hook := p.f.pullHook
	p.f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, p.ref); err != nil {
			return err
		}
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.images[p.ref] = true
	return nil
}

func (f *fakeEngine) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ImagePull", ref); err != nil {
		return nil, err
	}
	return &fakePull{Reader: strings.NewReader(""), f: f, ref: ref}, nil
}

func (f *fakeEngine) VolumeInspect(_ context.Context, name string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("VolumeInspect", name); err != nil {
		return client.VolumeInspectResult{}, err
	}
	v := f.volumes[name]
	if v == nil {
		return client.VolumeInspectResult{}, notFound("volume", name)
	}
	return client.VolumeInspectResult{Volume: volume.Volume{Name: name, Labels: maps.Clone(v.labels), Options: maps.Clone(v.driverOpts)}}, nil
}

func (f *fakeEngine) VolumeCreate(_ context.Context, o client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("VolumeCreate", o.Name); err != nil {
		return client.VolumeCreateResult{}, err
	}
	if f.volumes[o.Name] != nil {
		return client.VolumeCreateResult{}, fmt.Errorf("volume %s exists: %w", o.Name, cerrdefs.ErrConflict)
	}
	f.volumes[o.Name] = &fakeVolume{labels: maps.Clone(o.Labels), driverOpts: maps.Clone(o.DriverOpts)}
	return client.VolumeCreateResult{Volume: volume.Volume{Name: o.Name, Labels: o.Labels}}, nil
}

func (f *fakeEngine) VolumeRemove(_ context.Context, name string, _ client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("VolumeRemove", name); err != nil {
		return client.VolumeRemoveResult{}, err
	}
	if f.volumes[name] == nil {
		return client.VolumeRemoveResult{}, notFound("volume", name)
	}
	for _, c := range f.containers {
		for _, m := range c.opts.HostConfig.Mounts {
			if m.Source == name {
				return client.VolumeRemoveResult{}, fmt.Errorf("volume %s is in use by %s: %w", name, c.name, cerrdefs.ErrConflict)
			}
		}
	}
	delete(f.volumes, name)
	return client.VolumeRemoveResult{}, nil
}

func (f *fakeEngine) volume(name string) *fakeVolume {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumes[name]
}

func (f *fakeEngine) anonVolumes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.volumes {
		if v.anon {
			n++
		}
	}
	return n
}

var _ Engine = (*fakeEngine)(nil)
