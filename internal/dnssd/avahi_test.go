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

	"github.com/ThePhaseless/printscan/internal/device"
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
		f.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return &dbus.Call{Err: err}
		}
		r, ok := f.resolve[key]
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

func TestBrowseMapsLocalServices(t *testing.T) {
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
	got, err := newBrowser(f).browse(testCtx(t), device.BrowseTypes, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := []device.DNSSDService{
		{Type: device.ServiceIPP, Name: "Living room printer", Host: "node.local", Port: 8631, TXT: map[string]string{"rp": "ipp/print"}},
		{Type: device.ServiceIPP, Name: "Other printer", Host: "node.local", Port: 631, TXT: map[string]string{"rp": "ipp/print", "ty": "Other"}},
		{Type: device.ServiceUScan, Name: "Scanner", Host: "node.local", Port: 8090, TXT: map[string]string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("browse =\n%+v\nwant\n%+v", got, want)
	}
	// The IPv4 copy of an already resolved instance and the removed item are never resolved.
	wantResolves := []string{
		"_ipp._tcp/Living room printer/1", "_ipp._tcp/Other printer/0", "_ipp._tcp/Neighbour/0",
		"_ipp._tcp/Stale/0", "_ipp._tcp/Hung/0", "_uscan._tcp/Scanner/0",
	}
	if !reflect.DeepEqual(f.resolves, wantResolves) {
		t.Errorf("resolves = %v, want %v", f.resolves, wantResolves)
	}
	assertFreed(t, f, 2)
}

func TestBrowseNoTypes(t *testing.T) {
	f := &fakeAvahi{signals: make(chan *dbus.Signal, 1)}
	got, err := newBrowser(f).browse(testCtx(t), nil, time.Hour)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("browse(nil) = %#v, %v; want empty non-nil slice", got, err)
	}
}

func TestBrowseStopsAtWait(t *testing.T) {
	f := &fakeAvahi{
		signals: make(chan *dbus.Signal, 64),
		onBrowse: func(p dbus.ObjectPath, typ string) []*dbus.Signal {
			return []*dbus.Signal{itemSig(p, "ItemNew", 0, "Other printer", typ)} // never AllForNow
		},
		resolve: map[string]resolved{
			"_ipp._tcp/Other printer/0": {host: "node.local", addr: "192.168.1.20", port: 631},
		},
	}
	start := time.Now()
	got, err := newBrowser(f).browse(testCtx(t), []string{device.ServiceIPP}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Other printer" {
		t.Errorf("browse = %+v", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("browse took %v", d)
	}
	assertFreed(t, f, 1)
}

func TestBrowseFailureSignal(t *testing.T) {
	f := &fakeAvahi{
		signals: make(chan *dbus.Signal, 64),
		onBrowse: func(p dbus.ObjectPath, _ string) []*dbus.Signal {
			return []*dbus.Signal{sig(p, "Failure", "Bad state")}
		},
	}
	_, err := newBrowser(f).browse(testCtx(t), []string{device.ServiceIPP}, time.Hour)
	if err == nil {
		t.Fatal("want error from Failure signal")
	}
	assertFreed(t, f, 1)
}

func TestBrowseBrowserNewError(t *testing.T) {
	f := &fakeAvahi{
		signals:   make(chan *dbus.Signal, 64),
		browseErr: map[string]error{device.ServiceUScan: errors.New("org.freedesktop.Avahi.TooManyObjectsError")},
	}
	_, err := newBrowser(f).browse(testCtx(t), device.BrowseTypes, time.Hour)
	if err == nil {
		t.Fatal("want error from ServiceBrowserNew")
	}
	assertFreed(t, f, 1) // the _ipp._tcp browser created before the failure
}

func TestBrowseContextCancel(t *testing.T) {
	f := &fakeAvahi{signals: make(chan *dbus.Signal, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := newBrowser(f).browse(ctx, []string{device.ServiceIPP}, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertFreed(t, f, 1)
}

func TestBrowseClosedSignalChannel(t *testing.T) {
	f := &fakeAvahi{signals: make(chan *dbus.Signal, 64)}
	close(f.signals)
	if _, err := newBrowser(f).browse(testCtx(t), nil, time.Hour); err != nil {
		t.Fatalf("no types: %v", err)
	}
	f.onBrowse = nil
	b := newBrowser(f)
	if _, err := b.browse(testCtx(t), []string{device.ServiceIPP}, time.Hour); err == nil {
		t.Fatal("want error when the bus connection closes")
	}
	assertFreed(t, f, 1)
}

func assertFreed(t *testing.T, f *fakeAvahi, n int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.freed) != n {
		t.Errorf("freed %v, want %d browsers", f.freed, n)
	}
}
