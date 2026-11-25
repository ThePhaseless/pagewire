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
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ThePhaseless/printscan/internal/device"
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
)

// busObject is the part of dbus.BusObject Browse uses; tests fake it.
type busObject interface {
	CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call
}

// Browse browses the given service types in the "local" domain for at most wait
// (less once every browser reports AllForNow), then resolves each instance (at most
// resolveTimeout each) and returns those whose address is one of this host's
// interface addresses, sorted by type and name. It never closes conn.
func Browse(ctx context.Context, conn *dbus.Conn, types []string, wait time.Duration) ([]device.DNSSDService, error) {
	local, err := localAddrs()
	if err != nil {
		return nil, err
	}
	// Subscribe before creating browsers: Avahi may emit ItemNew before ServiceBrowserNew returns.
	match := []dbus.MatchOption{dbus.WithMatchSender(avahiService), dbus.WithMatchInterface(browserIface)}
	if err := conn.AddMatchSignalContext(ctx, match...); err != nil {
		return nil, fmt.Errorf("avahi: add signal match: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.RemoveMatchSignalContext(cctx, match...)
	}()
	// Large buffer: godbus delivers overflow from goroutines, which can reorder signals.
	ch := make(chan *dbus.Signal, 256)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	b := &browser{
		server:  conn.Object(avahiService, "/"),
		object:  func(p dbus.ObjectPath) busObject { return conn.Object(avahiService, p) },
		signals: ch,
		local:   local,
	}
	return b.browse(ctx, types, wait)
}

type browser struct {
	server  busObject
	object  func(dbus.ObjectPath) busObject
	signals <-chan *dbus.Signal
	local   map[netip.Addr]bool
}

type item struct {
	iface, proto      int32
	name, typ, domain string
}

func (b *browser) browse(ctx context.Context, types []string, wait time.Duration) ([]device.DNSSDService, error) {
	out := []device.DNSSDService{}
	if len(types) == 0 {
		return out, nil
	}

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
			return nil, fmt.Errorf("avahi: browse %s: %w", typ, err)
		}
		browsers[p] = typ
	}

	items, err := b.collect(ctx, browsers, wait)
	if err != nil {
		return nil, err
	}

	done := map[[2]string]bool{}
	for _, it := range items {
		key := [2]string{it.typ, it.name}
		if done[key] {
			continue // same instance on another interface or protocol
		}
		svc, err := b.resolve(ctx, it)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue // vanished or stale instance: skip, keep the rest
		}
		if svc != nil {
			done[key] = true
			out = append(out, *svc)
		}
	}
	slices.SortFunc(out, func(a, c device.DNSSDService) int {
		return cmp.Or(cmp.Compare(a.Type, c.Type), cmp.Compare(a.Name, c.Name))
	})
	return out, nil
}

// collect gathers ItemNew/ItemRemove signals for the given browsers until each has
// sent AllForNow or wait passes.
func (b *browser) collect(ctx context.Context, browsers map[dbus.ObjectPath]string, wait time.Duration) ([]item, error) {
	pending := maps.Clone(browsers) // until AllForNow
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var items []item
	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return items, nil
		case s, ok := <-b.signals:
			if !ok {
				return nil, errors.New("avahi: bus connection closed")
			}
			typ, mine := browsers[s.Path]
			if !mine {
				continue
			}
			switch strings.TrimPrefix(s.Name, browserIface+".") {
			case "ItemNew":
				var it item
				var flags uint32
				// Only instances this host announced: another LAN host can
				// announce any address, this node's included.
				if dbus.Store(s.Body, &it.iface, &it.proto, &it.name, &it.typ, &it.domain, &flags) == nil && flags&lookupResultLocal != 0 {
					items = append(items, it)
				}
			case "ItemRemove":
				var it item
				var flags uint32
				if dbus.Store(s.Body, &it.iface, &it.proto, &it.name, &it.typ, &it.domain, &flags) == nil {
					items = slices.DeleteFunc(items, func(x item) bool { return x == it })
				}
			case "Failure":
				return nil, fmt.Errorf("avahi: browse %s failed: %v", typ, s.Body)
			case "AllForNow":
				delete(pending, s.Path)
			}
		}
	}
	return items, nil
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
