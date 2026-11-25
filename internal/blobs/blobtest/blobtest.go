// Package blobtest is a synthetic vendor for blob fetching: an ephemeral
// signing key, a fake HPLIP plug-in archive and an in-memory HTTP transport.
// It holds no proprietary data. Tests use it, and so does the sim backend at
// runtime, so nothing here may touch the network.
package blobtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/ThePhaseless/printscan/internal/blobs"
)

// NewKey returns a fresh Ed25519 signing entity. It panics only if key
// generation fails, which with crypto/rand cannot happen in practice.
func NewKey() *openpgp.Entity {
	e, err := openpgp.NewEntity("printscan synthetic vendor", "", "vendor@printscan.invalid",
		&packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		panic("blobtest: generate key: " + err.Error())
	}
	return e
}

// Sign returns an armored detached signature of data made with e.
func Sign(e *openpgp.Entity, data []byte) []byte {
	var b bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&b, e, bytes.NewReader(data), nil); err != nil {
		panic("blobtest: sign: " + err.Error())
	}
	return b.Bytes()
}

// Spec is the plugin.spec inside Archive, in the layout hplip 3.24.4's
// installer/pluginhandler.py reads: [products] maps each product to sections,
// and each section names a payload file (src), its target (trg) and an
// optional symlink (link), with $VARIABLES substituted.
const Spec = `[plugin]
name=printscan synthetic plug-in

[products]
hp-laserjet_1018=lj,fw-1018,rules-1018
hp-laserjet_professional_m1132_mfp=lj,scan,fax

[lj]
src=lj-$ARCH.so
trg=$HOMEDIR/prnt/plugins/lj-$ARCH.so
link=$HOMEDIR/prnt/plugins/lj.so

[scan]
src=bb_soapht-$ARCH.so
trg=$HOMEDIR/scan/plugins/bb_soapht-$ARCH.so
link=$HOMEDIR/scan/plugins/bb_soapht.so

[fax]
src=fax_marvell-$ARCH.so
trg=$HOMEDIR/fax/plugins/fax_marvell-$ARCH.so
link=$HOMEDIR/fax/plugins/fax_marvell.so

[fw-1018]
src=hp_laserjet_1018.fw.gz
trg=$HOMEDIR/data/firmware/hp_laserjet_1018.fw.gz

[rules-1018]
src=86-hpmud-hp_laserjet_1018.rules
trg=$RULESDIR/86-hpmud-hp_laserjet_1018.rules
`

// License is the licence text at ./license.txt in Archive, the path the
// catalog's hplip-plugin EULA names.
const License = "synthetic licence\n"

// Archive returns a makeself-style .run file whose payload holds Spec, an
// arch-independent firmware file, a udev rule, and the lj, bb_soapht and
// fax_marvell libraries built for goarch only. A goarch HPLIP has no name for
// gets no libraries.
func Archive(goarch string) []byte {
	files := []struct{ name, body string }{
		{"./plugin.spec", Spec},
		{"./hp_laserjet_1018.fw.gz", "synthetic firmware"},
		{"./86-hpmud-hp_laserjet_1018.rules", "# synthetic udev rule\n"},
		{"./license.txt", License},
	}
	if arch, ok := blobs.HPLIPArch(goarch); ok {
		for _, lib := range []string{"lj", "bb_soapht", "fax_marvell"} {
			files = append(files, struct{ name, body string }{
				"./" + lib + "-" + arch + ".so", "synthetic " + lib + " for " + arch,
			})
		}
	}
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	tw := tar.NewWriter(gz)
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755, ModTime: time.Unix(0, 0)}))
	for _, f := range files {
		must(tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: f.name, Mode: 0o644, Size: int64(len(f.body)), ModTime: time.Unix(0, 0),
		}))
		_, err := io.WriteString(tw, f.body)
		must(err)
	}
	must(tw.Close())
	must(gz.Close())
	return Makeself(payload.Bytes())
}

// Makeself prepends a makeself 2.1-style shell header to a tar.gz payload. The
// header's `offset=` line counts the header lines, as makeself writes it.
func Makeself(payload []byte) []byte {
	lines := []string{
		"#!/bin/sh",
		"# This script was generated using Makeself 2.1.5",
		`CRCsum="0000000000"`,
		`MD5="00000000000000000000000000000000"`,
		`TMPROOT=${TMPDIR:=/tmp}`,
		`label="printscan synthetic HPLIP plug-in"`,
		`script="./hplip-plugin-install"`,
		`targetdir="plugin_tmp"`,
		fmt.Sprintf(`filesizes="%d"`, len(payload)),
		`keep=y`,
		"", // offset line, filled in below
		`echo "synthetic archive: nothing to run"`,
		`exit 0`,
	}
	lines[10] = fmt.Sprintf(`offset=`+"`"+`head -n %d "$0" | wc -c | tr -d " "`+"`", len(lines))
	return append([]byte(strings.Join(lines, "\n")+"\n"), payload...)
}

// Transport serves each URL in files (keyed by the full URL string) from
// memory and answers 404 for anything else. It never opens a connection.
func Transport(files map[string][]byte) http.RoundTripper {
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		body, ok := files[r.URL.String()]
		status := http.StatusOK
		if !ok {
			status, body = http.StatusNotFound, []byte("not found\n")
		}
		return &http.Response{
			StatusCode:    status,
			Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": {"application/octet-stream"}},
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       r,
		}, nil
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// must panics on writes to in-memory buffers, which cannot fail.
func must(err error) {
	if err != nil {
		panic("blobtest: " + err.Error())
	}
}
