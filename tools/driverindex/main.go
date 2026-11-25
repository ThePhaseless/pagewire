// Command driverindex builds internal/catalog/drivers.tsv.gz from the
// `<app> drivers` dumps of the PAPPL printer applications.
//
//	driverindex -in ps=ps.txt -in legacy=legacy.txt -o drivers.tsv.gz
//
// Each dump line is `name "description" "IEEE-1284 device ID"`. The output is
// a gzipped UTF-8 TSV: the header line, then one row per driver sorted by
// family and name, with the columns family, name, recommended (0/1), mfg,
// mdl, cmd and description. Drivers that need hpijs, which no image ships, are
// left out.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
)

const header = "# pagewire driver index v1"

type row struct {
	family, name, mfg, mdl, cmd, desc string
	recommended, hpijs                bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "driverindex:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("driverindex", flag.ContinueOnError)
	inputs := map[string]string{}
	fs.Func("in", "family=path of an `<app> drivers` dump (repeatable)", func(v string) error {
		family, path, ok := strings.Cut(v, "=")
		if !ok || family == "" || path == "" || strings.ContainsAny(family, "\t\r\n") {
			return fmt.Errorf("want family=path, got %q", v)
		}
		if _, dup := inputs[family]; dup {
			return fmt.Errorf("family %q given twice", family)
		}
		inputs[family] = path
		return nil
	})
	out := fs.String("o", "", "output file (gzipped TSV)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(inputs) == 0 || *out == "" || fs.NArg() > 0 {
		return errors.New("usage: driverindex -in family=path [-in ...] -o out.tsv.gz")
	}

	var rows []row
	for family, path := range inputs {
		r, err := readDump(family, path)
		if err != nil {
			return err
		}
		rows = append(rows, r...)
	}
	slices.SortFunc(rows, func(a, b row) int {
		if c := strings.Compare(a.family, b.family); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})

	var tsv bytes.Buffer
	tsv.WriteString(header + "\n")
	for i, r := range rows {
		if i > 0 && rows[i-1].family == r.family && rows[i-1].name == r.name {
			return fmt.Errorf("family %s lists driver %s twice", r.family, r.name)
		}
		rec := "0"
		if r.recommended {
			rec = "1"
		}
		tsv.WriteString(strings.Join([]string{r.family, r.name, rec, r.mfg, r.mdl, r.cmd, r.desc}, "\t") + "\n")
	}

	// The zero gzip.Header (no name, no mtime) keeps the output reproducible.
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(tsv.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(*out, gz.Bytes(), 0o644) //nolint:gosec // a build artifact that gets committed, not a secret
}

func readDump(family, path string) ([]row, error) {
	f, err := os.Open(path) //nolint:gosec // the path is a build input named on the command line
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var rows []row
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		r, err := parseLine(family, sc.Text())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if !r.hpijs {
			rows = append(rows, r)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no drivers", path)
	}
	return rows, nil
}

// parseLine reads one `name "description" "device ID"` line.
func parseLine(family, line string) (row, error) {
	name, rest, _ := strings.Cut(line, " ")
	i := strings.LastIndex(rest, `" "`)
	if name == "" || strings.Contains(name, `"`) || i < 0 ||
		!strings.HasPrefix(rest, `"`) || !strings.HasSuffix(rest, `"`) || len(rest) < i+4 {
		return row{}, fmt.Errorf(`want name "description" "device ID", got %q`, line)
	}
	desc, devid := rest[1:i], rest[i+3:len(rest)-1]
	id := parseDeviceID(devid)
	return row{
		family:      family,
		name:        clean(name),
		mfg:         clean(id["MFG"]),
		mdl:         clean(id["MDL"]),
		cmd:         clean(id["CMD"]),
		desc:        clean(desc),
		recommended: strings.Contains(name, "-recommended-") || slices.Contains(strings.Split(id["DRV"], ","), "R1"),
		// Foomatic drivers whose DRV back end is HPLIP's hpijs IJS server: no image
		// ships it, so they are listed but cannot print and stay out of the index.
		hpijs: strings.HasPrefix(id["DRV"], "Dhpijs"),
	}, nil
}

// parseDeviceID mirrors device.ParseDeviceID, which this stdlib-only tool cannot import.
func parseDeviceID(s string) map[string]string {
	aliases := map[string]string{"MANUFACTURER": "MFG", "MODEL": "MDL", "COMMAND SET": "CMD"}
	out := map[string]string{}
	for field := range strings.SplitSeq(s, ";") {
		k, v, ok := strings.Cut(field, ":")
		if !ok {
			continue
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		if a, ok := aliases[k]; ok {
			k = a
		}
		if _, seen := out[k]; k == "" || seen {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// clean turns tabs and line breaks into spaces so a field cannot split a row.
func clean(s string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(s)
}
