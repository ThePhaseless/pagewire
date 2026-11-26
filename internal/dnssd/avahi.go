// Package dnssd browses and resolves DNS-SD services through Avahi's D-Bus API,
// keeping only services published from this host.
package dnssd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ThePhaseless/pagewire/internal/device"
)

const (
	avahiService = "org.freedesktop.Avahi"
	serverIface  = avahiService + ".Server"
	browserIface = avahiService + ".ServiceBrowser"

	ifaceUnspec int32 = -1
	protoUnspec int32 = -1

	// lookupResultLocal is AVAHI_LOOKUP_RESULT_LOCAL: the service browser
	// sets it on ItemNew for records this host announced.
	lookupResultLocal uint32 = 8

	resolveTimeout = 2 * time.Second

	busService       = "org.freedesktop.DBus"
	nameOwnerChanged = busService + ".NameOwnerChanged"

	// watchInitial bounds Watch's wait for every browser's first AllForNow.
	watchInitial = 2 * time.Second
	// watchSettle debounces Watch's later updates: Avahi announces in bursts.
	watchSettle = 250 * time.Millisecond
	// resolveRetry and resolveRetryMax bound Watch's retries of failed
	// resolves (Avahi still busy at start-up), which no signal would trigger.
	resolveRetry    = time.Second
	resolveRetryMax = 30 * time.Second
)

// busObject is the part of dbus.BusObject Watch uses; tests fake it.
type busObject interface {
	CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call
}

// Watch runs one long-lived ServiceBrowser per type in the "local" domain and calls
// update with this host's services, sorted by type and name, once every browser has sent
// its first AllForNow or watchInitial passes, then after each change to the instance
// set, settled for watchSettle and only when the list differs. Instances whose resolve
// failed are resolved again after 1 s, doubling to 30 s. It returns ctx.Err() when
// ctx ends, and an error when Avahi's bus name changes owner (restart or exit), a
// browser fails, or the bus connection closes: the browsers are gone then, so the caller
// starts a new Watch. It frees its browsers on return and never closes conn.
//
// shortcut: a TXT change of an instance already seen is not re-resolved (the keys pagewire
// reads, usb_MFG, usb_MDL and ty, are static); re-resolve on a timer if that starts to matter.
func Watch(ctx context.Context, conn *dbus.Conn, types []string, update func([]device.DNSSDService)) error {
	local, err := localAddrs()
	if err != nil {
		return err
	}
	// Subscribe before creating browsers: Avahi may emit ItemNew before ServiceBrowserNew returns.
	matches := [][]dbus.MatchOption{
		{dbus.WithMatchSender(avahiService), dbus.WithMatchInterface(browserIface)},
		{
			dbus.WithMatchSender(busService), dbus.WithMatchInterface(busService),
			dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, avahiService),
		},
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		for _, m := range matches {
			_ = conn.RemoveMatchSignalContext(cctx, m...)
		}
	}()
	for _, m := range matches {
		if err := conn.AddMatchSignalContext(ctx, m...); err != nil {
			return fmt.Errorf("avahi: add signal match: %w", err)
		}
	}
	// Large buffer: godbus delivers overflow from goroutines, which can reorder signals.
	ch := make(chan *dbus.Signal, 256)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	b := &browser{
		server:  conn.Object(avahiService, "/"),
		object:  func(p dbus.ObjectPath) busObject { return conn.Object(avahiService, p) },
		signals: ch,
		local:   local,
		addrs:   localAddrs,
	}
	return b.watch(ctx, types, watchInitial, update)
}

type browser struct {
	server  busObject
	object  func(dbus.ObjectPath) busObject
	signals <-chan *dbus.Signal
	local   map[netip.Addr]bool
	// addrs, when set, refreshes local before each Watch update: addresses change while it runs.
	addrs func() (map[netip.Addr]bool, error)
	// retry is the first wait before Watch resolves again the instances whose
	// resolve failed; it doubles to resolveRetryMax. 0: resolveRetry.
	retry time.Duration
}

type item struct {
	iface, proto      int32
	name, typ, domain string
}

// resolve returns nil without error when the instance resolves to a non-local address.
func (b *browser) resolve(ctx context.Context, it item) (*device.DNSSDService, error) {
	var (
		iface, proto, aproto int32
		name, typ, domain    string
		host, addr           string
		port                 uint16
		txt                  [][]byte
		flags                uint32
	)
	// Local records answer at once; a stale LAN entry would otherwise hold the caller for Avahi's own timeout.
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	err := b.server.CallWithContext(ctx, serverIface+".ResolveService", 0,
		it.iface, it.proto, it.name, it.typ, it.domain, protoUnspec, uint32(0)).
		Store(&iface, &proto, &name, &typ, &domain, &host, &aproto, &addr, &port, &txt, &flags)
	if err != nil {
		return nil, err
	}
	if !isLocal(b.local, addr) {
		return nil, nil
	}
	return &device.DNSSDService{Type: it.typ, Name: it.name, Host: host, Port: int(port), TXT: parseTXT(txt)}, nil
}

// parseTXT decodes TXT records per RFC 6763 section 6: key=value, a bare key is an
// empty value, records without a key are ignored, and only a key's first occurrence counts.
func parseTXT(recs [][]byte) map[string]string {
	m := make(map[string]string, len(recs))
	for _, r := range recs {
		k, v, _ := strings.Cut(string(r), "=")
		if k == "" {
			continue
		}
		if _, dup := m[k]; !dup {
			m[k] = v
		}
	}
	return m
}

func isLocal(local map[netip.Addr]bool, addr string) bool {
	a, err := netip.ParseAddr(addr)
	return err == nil && local[a.Unmap().WithZone("")]
}

func localAddrs() (map[netip.Addr]bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("dnssd: interface addresses: %w", err)
	}
	m := make(map[netip.Addr]bool, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				m[ip.Unmap()] = true
			}
		}
	}
	return m, nil
}

func (b *browser) watch(ctx context.Context, types []string, initial time.Duration, update func([]device.DNSSDService)) error {
	browsers := map[dbus.ObjectPath]string{} // browser path -> type
	defer func() {
		// Free also after ctx is cancelled: leaked browsers count against Avahi's per-client limit.
		// Not FlagNoReplyExpected: on a closed conn godbus returns a Call whose nil Done blocks forever.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		for p := range browsers {
			b.object(p).CallWithContext(fctx, browserIface+".Free", 0)
		}
	}()
	for _, typ := range types {
		var p dbus.ObjectPath
		err := b.server.CallWithContext(ctx, serverIface+".ServiceBrowserNew", 0,
			ifaceUnspec, protoUnspec, typ, "local", uint32(0)).Store(&p)
		if err != nil {
			return fmt.Errorf("avahi: browse %s: %w", typ, err)
		}
		browsers[p] = typ
	}

	var (
		pending  = maps.Clone(browsers)           // until the first AllForNow
		items    []item                           // local instances in arrival order
		resolved = map[item]device.DNSSDService{} // local results only: addresses can change
		last     []device.DNSSDService
		started  bool
	)
	retryMin := cmp.Or(b.retry, resolveRetry)
	retry := retryMin
	// publish reports whether any resolve failed.
	publish := func() (bool, error) {
		failed := false
		if b.addrs != nil {
			if m, err := b.addrs(); err == nil {
				b.local = m
			}
		}
		out := []device.DNSSDService{}
		done := map[[2]string]bool{}
		for _, it := range items {
			key := [2]string{it.typ, it.name}
			if done[key] {
				continue // same instance on another interface or protocol
			}
			svc, ok := resolved[it]
			if !ok {
				r, err := b.resolve(ctx, it)
				if err != nil {
					if ctx.Err() != nil {
						return false, ctx.Err()
					}
					failed = true // vanished or stale: retried with backoff or at the next change
					continue
				}
				if r == nil {
					continue // another host's address, for now
				}
				svc, resolved[it] = *r, *r
			}
			done[key] = true
			out = append(out, svc)
		}
		slices.SortFunc(out, func(a, c device.DNSSDService) int {
			return cmp.Or(cmp.Compare(a.Type, c.Type), cmp.Compare(a.Name, c.Name))
		})
		if !started || !reflect.DeepEqual(out, last) {
			started, last = true, out
			update(slices.Clone(out))
		}
		return failed, nil
	}

	deadline := time.NewTimer(initial)
	defer deadline.Stop()
	settle := time.NewTimer(watchSettle)
	settle.Stop()
	defer settle.Stop()
	// publishRetry publishes and, when a resolve failed, arms settle to try
	// again after the backoff.
	publishRetry := func() error {
		failed, err := publish()
		switch {
		case err != nil:
			return err
		case failed:
			settle.Reset(retry)
			retry = min(2*retry, resolveRetryMax)
		default:
			retry = retryMin
		}
		return nil
	}
	for {
		if !started && len(pending) == 0 {
			deadline.Stop()
			if err := publishRetry(); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			clear(pending)
		case <-settle.C:
			if err := publishRetry(); err != nil {
				return err
			}
		case s, ok := <-b.signals:
			if !ok {
				return errors.New("avahi: bus connection closed")
			}
			if s.Name == nameOwnerChanged {
				if len(s.Body) > 0 && s.Body[0] == avahiService {
					return errors.New("avahi: daemon restarted or exited")
				}
				continue
			}
			typ, mine := browsers[s.Path]
			if !mine {
				continue
			}
			changed := false
			switch strings.TrimPrefix(s.Name, browserIface+".") {
			case "ItemNew":
				var it item
				var flags uint32
				// Only instances this host announced: another LAN host can
				// announce any address, this node's included.
				if dbus.Store(s.Body, &it.iface, &it.proto, &it.name, &it.typ, &it.domain, &flags) == nil &&
					flags&lookupResultLocal != 0 && !slices.Contains(items, it) {
					items, changed = append(items, it), true
				}
			case "ItemRemove":
				var it item
				var flags uint32
				if dbus.Store(s.Body, &it.iface, &it.proto, &it.name, &it.typ, &it.domain, &flags) == nil {
					if i := slices.Index(items, it); i >= 0 {
						items, changed = slices.Delete(items, i, i+1), true
						delete(resolved, it)
					}
				}
			case "Failure":
				return fmt.Errorf("avahi: browse %s failed: %v", typ, s.Body)
			case "AllForNow":
				delete(pending, s.Path)
			}
			if changed && started {
				settle.Reset(watchSettle)
			}
		}
	}
}
