package dnssd

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ThePhaseless/pagewire/internal/device"
)

func TestParseTXT(t *testing.T) {
	got := parseTXT([][]byte{
		[]byte("rp=ipp/print"),
		[]byte("ty=HP LaserJet"),
		[]byte("rp=second"), // RFC 6763 6.4: only the first occurrence counts
		[]byte("Color"),     // boolean attribute
		[]byte("pdl=a=b"),   // value keeps later '='
		[]byte("=orphan"),   // no key: ignored
		{},
	})
	want := map[string]string{"rp": "ipp/print", "ty": "HP LaserJet", "Color": "", "pdl": "a=b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseTXT = %v, want %v", got, want)
	}
	if m := parseTXT(nil); m == nil || len(m) != 0 {
		t.Errorf("parseTXT(nil) = %#v, want empty non-nil map", m)
	}
}

func TestIsLocal(t *testing.T) {
	local := map[netip.Addr]bool{
		netip.MustParseAddr("192.168.1.20"): true,
		netip.MustParseAddr("fe80::1"):      true,
	}
	for addr, want := range map[string]bool{
		"192.168.1.20":        true,
		"::ffff:192.168.1.20": true,
		"fe80::1%eth0":        true,
		"fe80::1":             true,
		"192.168.1.99":        false,
		"":                    false,
		"not-an-ip":           false,
	} {
		if got := isLocal(local, addr); got != want {
			t.Errorf("isLocal(%q) = %v, want %v", addr, got, want)
		}
	}
}

// resolved is one scripted ResolveService reply.
type resolved struct {
	host, addr string
	port       uint16
	txt        [][]byte
	err        error
	block      bool // wait for the call's ctx, like a stale LAN entry
}

// fakeAvahi plays the Avahi server object and the browser objects.
type fakeAvahi struct {
	signals chan *dbus.Signal
	// onBrowse returns the signals the browser for a type emits right after creation
	// (before ServiceBrowserNew returns, as the real daemon may).
	onBrowse  func(path dbus.ObjectPath, typ string) []*dbus.Signal
	browseErr map[string]error
	resolve   map[string]resolved // by "type/name/proto"

	mu       sync.Mutex
	n        int
	resolves []string
	freed    []dbus.ObjectPath
}

func (f *fakeAvahi) CallWithContext(ctx context.Context, method string, _ dbus.Flags, args ...any) *dbus.Call {
	switch method {
	case serverIface + ".ServiceBrowserNew":
		typ := args[2].(string)
		if err := f.browseErr[typ]; err != nil {
			return &dbus.Call{Err: err}
		}
		f.mu.Lock()
		f.n++
		path := dbus.ObjectPath(fmt.Sprintf("/Client1/ServiceBrowser%d", f.n))
		f.mu.Unlock()
		if f.onBrowse != nil {
			for _, s := range f.onBrowse(path, typ) {
				f.signals <- s
			}
		}
		return &dbus.Call{Body: []any{path}}
	case serverIface + ".ResolveService":
		iface, proto, name, typ, domain := args[0].(int32), args[1].(int32), args[2].(string), args[3].(string), args[4].(string)
		key := fmt.Sprintf("%s/%s/%d", typ, name, proto)
		f.mu.Lock()
		f.resolves = append(f.resolves, key)
		r, ok := f.resolve[key]
		f.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return &dbus.Call{Err: err}
		}
		if r.block {
			<-ctx.Done()
			return &dbus.Call{Err: ctx.Err()}
		}
		if !ok || r.err != nil {
			return &dbus.Call{Err: errors.Join(r.err, errors.New("org.freedesktop.Avahi.TimeoutError"))}
		}
		return &dbus.Call{Body: []any{
			iface, proto, name, typ, domain, r.host, int32(0), r.addr, r.port, r.txt, uint32(0),
		}}
	}
	return &dbus.Call{Err: fmt.Errorf("unexpected method %s", method)}
}

func (f *fakeAvahi) object(path dbus.ObjectPath) busObject { return freer{f, path} }

type freer struct {
	f    *fakeAvahi
	path dbus.ObjectPath
}

func (o freer) CallWithContext(_ context.Context, method string, _ dbus.Flags, _ ...any) *dbus.Call {
	if method != browserIface+".Free" {
		return &dbus.Call{Err: fmt.Errorf("unexpected method %s", method)}
	}
	o.f.mu.Lock()
	o.f.freed = append(o.f.freed, o.path)
	o.f.mu.Unlock()
	return &dbus.Call{}
}

func sig(path dbus.ObjectPath, member string, body ...any) *dbus.Signal {
	return &dbus.Signal{Sender: ":1.5", Path: path, Name: browserIface + "." + member, Body: body}
}

// itemSig is a browser signal for an instance this host announced
// (AVAHI_LOOKUP_RESULT_MULTICAST | AVAHI_LOOKUP_RESULT_LOCAL).
func itemSig(path dbus.ObjectPath, member string, proto int32, name, typ string) *dbus.Signal {
	return sig(path, member, int32(2), proto, name, typ, "local", uint32(4|8))
}

// remoteItemSig is an instance announced by another host (multicast only).
func remoteItemSig(path dbus.ObjectPath, proto int32, name, typ string) *dbus.Signal {
	return sig(path, "ItemNew", int32(2), proto, name, typ, "local", uint32(4))
}

func newBrowser(f *fakeAvahi) *browser {
	return &browser{
		server:  f,
		object:  f.object,
		signals: f.signals,
		local: map[netip.Addr]bool{
			netip.MustParseAddr("192.168.1.20"): true,
			netip.MustParseAddr("fe80::1"):      true,
		},
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestWatchMapsLocalServices(t *testing.T) {
	f := &fakeAvahi{
		signals: make(chan *dbus.Signal, 64),
		onBrowse: func(p dbus.ObjectPath, typ string) []*dbus.Signal {
			switch typ {
			case device.ServiceIPP:
				return []*dbus.Signal{
					// another client's browser: ignored
					itemSig("/Client9/ServiceBrowser1", "ItemNew", 0, "Foreign", device.ServiceIPP),
					// IPv6 copy first; its link-local address with a zone is still local
					itemSig(p, "ItemNew", 1, "Living room printer", device.ServiceIPP),
					itemSig(p, "ItemNew", 0, "Living room printer", device.ServiceIPP),
					itemSig(p, "ItemNew", 0, "Other printer", device.ServiceIPP),
					itemSig(p, "ItemNew", 0, "Neighbour", device.ServiceIPP),
					// another host announcing this node's address: never resolved
					remoteItemSig(p, 0, "Spoofed", device.ServiceIPP),
					itemSig(p, "ItemNew", 0, "Stale", device.ServiceIPP),
					itemSig(p, "ItemNew", 0, "Hung", device.ServiceIPP),
					itemSig(p, "ItemNew", 0, "Removed", device.ServiceIPP),
					itemSig(p, "ItemRemove", 0, "Removed", device.ServiceIPP),
					sig(p, "CacheExhausted"),
					sig(p, "AllForNow"),
				}
			default:
				return []*dbus.Signal{
					itemSig(p, "ItemNew", 0, "Scanner", device.ServiceUScan),
					sig(p, "AllForNow"),
				}
			}
		},
		resolve: map[string]resolved{
			"_ipp._tcp/Living room printer/1": {host: "node.local", addr: "fe80::1%eth0", port: 8631, txt: [][]byte{[]byte("rp=ipp/print")}},
			"_ipp._tcp/Other printer/0":       {host: "node.local", addr: "192.168.1.20", port: 631, txt: [][]byte{[]byte("rp=ipp/print"), []byte("ty=Other")}},
			"_ipp._tcp/Neighbour/0":           {host: "neighbour.local", addr: "192.168.1.99", port: 631},
			"_ipp._tcp/Spoofed/0":             {host: "node.local", addr: "192.168.1.20", port: 60000, txt: [][]byte{[]byte("rp=ipp/print")}},
			"_ipp._tcp/Stale/0":               {err: errors.New("timeout")},
			"_ipp._tcp/Hung/0":                {block: true},
			"_uscan._tcp/Scanner/0":           {host: "node.local", addr: "192.168.1.20", port: 8090},
		},
	}
	updates, _ := startWatch(t, testCtx(t), f, device.BrowseTypes, time.Hour)
	got := nextUpdate(t, updates, 2*resolveTimeout+time.Second) // Hung holds its resolve for resolveTimeout
	want := []device.DNSSDService{
		{Type: device.ServiceIPP, Name: "Living room printer", Host: "node.local", Port: 8631, TXT: map[string]string{"rp": "ipp/print"}},
		{Type: device.ServiceIPP, Name: "Other printer", Host: "node.local", Port: 631, TXT: map[string]string{"rp": "ipp/print", "ty": "Other"}},
		{Type: device.ServiceUScan, Name: "Scanner", Host: "node.local", Port: 8090, TXT: map[string]string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("watch =\n%+v\nwant\n%+v", got, want)
	}
	// The IPv4 copy of an already resolved instance and the removed item are never resolved.
	wantResolves := []string{
		"_ipp._tcp/Living room printer/1", "_ipp._tcp/Other printer/0", "_ipp._tcp/Neighbour/0",
		"_ipp._tcp/Stale/0", "_ipp._tcp/Hung/0", "_uscan._tcp/Scanner/0",
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.resolves, wantResolves) {
		t.Errorf("resolves = %v, want %v", f.resolves, wantResolves)
	}
}

func assertFreed(t *testing.T, f *fakeAvahi, n int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.freed) != n {
		t.Errorf("freed %v, want %d browsers", f.freed, n)
	}
}

// startWatch runs watch in a goroutine; updates and the return value arrive on channels.
func startWatch(t *testing.T, ctx context.Context, f *fakeAvahi, types []string, initial time.Duration) (<-chan []device.DNSSDService, <-chan error) {
	t.Helper()
	return startWatchWith(ctx, newBrowser(f), types, initial)
}

func startWatchWith(ctx context.Context, b *browser, types []string, initial time.Duration) (<-chan []device.DNSSDService, <-chan error) {
	updates := make(chan []device.DNSSDService, 16)
	errc := make(chan error, 1)
	go func() {
		errc <- b.watch(ctx, types, initial, func(s []device.DNSSDService) { updates <- s })
	}()
	return updates, errc
}

func nextUpdate(t *testing.T, updates <-chan []device.DNSSDService, within time.Duration) []device.DNSSDService {
	t.Helper()
	select {
	case u := <-updates:
		return u
	case <-time.After(within):
		t.Fatalf("no update within %v", within)
		return nil
	}
}

func noUpdate(t *testing.T, updates <-chan []device.DNSSDService) {
	t.Helper()
	select {
	case u := <-updates:
		t.Fatalf("unexpected update %+v", u)
	case <-time.After(2 * watchSettle):
	}
}

func watchErr(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return")
		return nil
	}
}

func names(s []device.DNSSDService) []string {
	out := []string{}
	for _, x := range s {
		out = append(out, x.Name)
	}
	return out
}

func watchFake() *fakeAvahi {
	return &fakeAvahi{
		signals: make(chan *dbus.Signal, 64),
		onBrowse: func(p dbus.ObjectPath, typ string) []*dbus.Signal {
			if typ == device.ServiceIPP {
				return []*dbus.Signal{
					itemSig(p, "ItemNew", 0, "Other printer", typ),
					itemSig(p, "ItemNew", 0, "Neighbour", typ),
					sig(p, "AllForNow"),
				}
			}
			return []*dbus.Signal{itemSig(p, "ItemNew", 0, "Scanner", typ), sig(p, "AllForNow")}
		},
		resolve: map[string]resolved{
			"_ipp._tcp/Other printer/0": {host: "node.local", addr: "192.168.1.20", port: 631, txt: [][]byte{[]byte("ty=Other")}},
			"_ipp._tcp/Neighbour/0":     {host: "neighbour.local", addr: "192.168.1.99", port: 631},
			"_ipp._tcp/New printer/0":   {host: "node.local", addr: "192.168.1.20", port: 8631},
			"_ipp._tcp/Second/1":        {host: "node.local", addr: "fe80::1", port: 8632},
			"_ipp._tcp/Spoofed/0":       {host: "node.local", addr: "192.168.1.20", port: 60000},
			"_uscan._tcp/Scanner/0":     {host: "node.local", addr: "192.168.1.20", port: 8090},
		},
	}
}

func TestWatchInitialAfterAllForNow(t *testing.T) {
	f := watchFake()
	ctx, cancel := context.WithCancel(testCtx(t))
	updates, errc := startWatch(t, ctx, f, device.BrowseTypes, time.Hour)
	got := nextUpdate(t, updates, time.Second) // well before the hour-long deadline
	want := []device.DNSSDService{
		{Type: device.ServiceIPP, Name: "Other printer", Host: "node.local", Port: 631, TXT: map[string]string{"ty": "Other"}},
		{Type: device.ServiceUScan, Name: "Scanner", Host: "node.local", Port: 8090, TXT: map[string]string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("initial update =\n%+v\nwant\n%+v", got, want)
	}
	cancel()
	if err := watchErr(t, errc); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	assertFreed(t, f, 2)
}

func TestWatchInitialDeadline(t *testing.T) {
	f := watchFake()
	f.onBrowse = func(p dbus.ObjectPath, typ string) []*dbus.Signal {
		return []*dbus.Signal{itemSig(p, "ItemNew", 0, "Other printer", typ)} // never AllForNow
	}
	updates, _ := startWatch(t, testCtx(t), f, []string{device.ServiceIPP}, 50*time.Millisecond)
	if got := names(nextUpdate(t, updates, time.Second)); !reflect.DeepEqual(got, []string{"Other printer"}) {
		t.Errorf("initial update = %v", got)
	}
}

func TestWatchNoTypes(t *testing.T) {
	f := watchFake()
	updates, _ := startWatch(t, testCtx(t), f, nil, time.Hour)
	if got := nextUpdate(t, updates, time.Second); got == nil || len(got) != 0 {
		t.Errorf("initial update = %#v, want empty non-nil slice", got)
	}
}

func TestWatchLaterChanges(t *testing.T) {
	f := watchFake()
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	updates, _ := startWatch(t, ctx, f, []string{device.ServiceIPP}, time.Hour)
	nextUpdate(t, updates, time.Second)
	const p = dbus.ObjectPath("/Client1/ServiceBrowser1")

	// Two announcements in one burst: one update, well before the 2 s initial wait.
	start := time.Now()
	f.signals <- itemSig(p, "ItemNew", 0, "New printer", device.ServiceIPP)
	f.signals <- itemSig(p, "ItemNew", 1, "Second", device.ServiceIPP)
	if got := names(nextUpdate(t, updates, time.Second)); !reflect.DeepEqual(got, []string{"New printer", "Other printer", "Second"}) {
		t.Errorf("after ItemNew = %v", got)
	}
	if d := time.Since(start); d < watchSettle || d > watchInitial {
		t.Errorf("update after %v, want the %v settle", d, watchSettle)
	}

	// Nothing local changes: no update.
	f.signals <- remoteItemSig(p, 0, "Spoofed", device.ServiceIPP)
	f.signals <- itemSig("/Client9/ServiceBrowser1", "ItemNew", 0, "Foreign", device.ServiceIPP)
	f.signals <- itemSig(p, "ItemNew", 0, "Other printer", device.ServiceIPP) // already known
	f.signals <- sig(p, "AllForNow")
	f.signals <- &dbus.Signal{Name: nameOwnerChanged, Body: []any{"org.example.Other", "", ":1.9"}}
	noUpdate(t, updates)

	f.signals <- itemSig(p, "ItemRemove", 0, "Other printer", device.ServiceIPP)
	if got := names(nextUpdate(t, updates, time.Second)); !reflect.DeepEqual(got, []string{"New printer", "Second"}) {
		t.Errorf("after ItemRemove = %v", got)
	}

	// A non-local instance goes: the published list is the same.
	f.signals <- itemSig(p, "ItemRemove", 0, "Neighbour", device.ServiceIPP)
	noUpdate(t, updates)

	// Local results are resolved once; Neighbour (another host's address) at each
	// publish, as the local addresses may change; Spoofed (remote flag) never.
	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{
		"_ipp._tcp/Other printer/0", "_ipp._tcp/Neighbour/0",
		"_ipp._tcp/Neighbour/0", "_ipp._tcp/New printer/0", "_ipp._tcp/Second/1",
		"_ipp._tcp/Neighbour/0",
	}
	if !reflect.DeepEqual(f.resolves, want) {
		t.Errorf("resolves = %v, want %v", f.resolves, want)
	}
}

// A resolve that fails (Avahi busy at start-up) is retried with backoff, with
// no further signal.
func TestWatchRetriesFailedResolve(t *testing.T) {
	f := watchFake()
	f.mu.Lock()
	good := f.resolve["_ipp._tcp/Other printer/0"]
	f.resolve["_ipp._tcp/Other printer/0"] = resolved{err: errors.New("busy")}
	f.mu.Unlock()
	b := newBrowser(f)
	b.retry = 20 * time.Millisecond
	updates, _ := startWatchWith(testCtx(t), b, []string{device.ServiceIPP}, time.Hour)
	if got := names(nextUpdate(t, updates, time.Second)); len(got) != 0 {
		t.Fatalf("initial update = %v, want none resolved", got)
	}
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			f.mu.Lock()
			c := 0
			for _, r := range f.resolves {
				if r == "_ipp._tcp/Other printer/0" {
					c++
				}
			}
			f.mu.Unlock()
			if c >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d resolves of the failed instance, want %d", c, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(3) // the initial one and two retries
	f.mu.Lock()
	f.resolve["_ipp._tcp/Other printer/0"] = good
	f.mu.Unlock()
	if got := names(nextUpdate(t, updates, time.Second)); !reflect.DeepEqual(got, []string{"Other printer"}) {
		t.Errorf("update after the retry = %v", got)
	}
}

func TestWatchErrors(t *testing.T) {
	const p = dbus.ObjectPath("/Client1/ServiceBrowser1")
	for name, s := range map[string]*dbus.Signal{
		"avahi owner changed": {Sender: busService, Name: nameOwnerChanged, Body: []any{avahiService, ":1.5", ""}},
		"browser failure":     sig(p, "Failure", "Bad state"),
		"connection closed":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := watchFake()
			updates, errc := startWatch(t, testCtx(t), f, []string{device.ServiceIPP}, time.Hour)
			nextUpdate(t, updates, time.Second)
			if s == nil {
				close(f.signals)
			} else {
				f.signals <- s
			}
			if err := watchErr(t, errc); err == nil || errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want a watch error", err)
			}
			assertFreed(t, f, 1)
		})
	}
}

func TestWatchBrowserNewError(t *testing.T) {
	f := watchFake()
	f.browseErr = map[string]error{device.ServiceUScan: errors.New("org.freedesktop.Avahi.TooManyObjectsError")}
	updates, errc := startWatch(t, testCtx(t), f, device.BrowseTypes, time.Hour)
	if err := watchErr(t, errc); err == nil {
		t.Fatal("want error from ServiceBrowserNew")
	}
	if len(updates) != 0 {
		t.Error("update before every browser exists")
	}
	assertFreed(t, f, 1)
}

func TestWatchLocalAddressChange(t *testing.T) {
	f := watchFake()
	b := newBrowser(f)
	var mu sync.Mutex
	local := map[netip.Addr]bool{netip.MustParseAddr("192.168.1.20"): true}
	b.addrs = func() (map[netip.Addr]bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return local, nil
	}
	updates, _ := startWatchWith(testCtx(t), b, []string{device.ServiceIPP}, time.Hour)
	if got := names(nextUpdate(t, updates, time.Second)); !reflect.DeepEqual(got, []string{"Other printer"}) {
		t.Fatalf("initial update = %v", got)
	}
	// The node gains the address Neighbour resolved to; the next change picks it up.
	mu.Lock()
	local = map[netip.Addr]bool{netip.MustParseAddr("192.168.1.20"): true, netip.MustParseAddr("192.168.1.99"): true}
	mu.Unlock()
	f.signals <- itemSig("/Client1/ServiceBrowser1", "ItemNew", 0, "New printer", device.ServiceIPP)
	if got := names(nextUpdate(t, updates, time.Second)); !reflect.DeepEqual(got, []string{"Neighbour", "New printer", "Other printer"}) {
		t.Errorf("after address change = %v", got)
	}
}
