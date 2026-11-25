package catalog

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const (
	driverIndexHeader = "# pagewire driver index v1"
	saneIndexHeader   = "# pagewire sane usb index v1"
)

// Driver is one row of the driver index: a driver a PAPPL family's app lists.
type Driver struct {
	Family      string
	Name        string // PAPPL driver name, passed to Create-Printer
	Recommended bool
	MFG, MDL    string // from the driver's IEEE-1284 device ID; "" when absent
	CMD         string
	Description string
}

// DriverIndex is drivers.tsv.gz, with the vendor aliases and model keys the
// matcher derives from it. A nil *DriverIndex lists nothing.
type DriverIndex struct {
	rows      []Driver
	alias     map[string]string   // alnum(MFG) or make key -> make key
	aliasesOf map[string][]string // make key -> the alias keys that map to it
	byModel   map[[2]string][]int // {vendor, modelKey} -> row indexes
}

// ParseDriverIndex reads the plain (decompressed) driver index TSV.
func ParseDriverIndex(r io.Reader) (*DriverIndex, error) {
	x := &DriverIndex{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if n == 1 {
			if line != driverIndexHeader {
				return nil, fmt.Errorf("driver index: line 1: want %q", driverIndexHeader)
			}
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 7 || f[0] == "" || f[1] == "" || (f[2] != "0" && f[2] != "1") {
			return nil, fmt.Errorf("driver index: line %d: want family, name, 0|1, mfg, mdl, cmd, description", n)
		}
		x.rows = append(x.rows, Driver{Family: f[0], Name: f[1], Recommended: f[2] == "1", MFG: f[3], MDL: f[4], CMD: f[5], Description: f[6]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("driver index: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("driver index: line 1: want %q", driverIndexHeader)
	}
	x.build()
	return x, nil
}

// build learns the vendor aliases from the rows, then keys every row that has
// MFG and MDL by {vendor, model key}.
func (x *DriverIndex) build() {
	votes := map[string]map[string]int{}
	x.alias = map[string]string{}
	for _, d := range x.rows {
		mk := makeKey(d.Name)
		x.alias[mk] = mk
		if a := alnum(d.MFG); a != "" {
			if votes[a] == nil {
				votes[a] = map[string]int{}
			}
			votes[a][mk]++
		}
	}
	// The majority of a MFG's rows wins; a tie goes to the lexically smallest.
	for a, v := range votes {
		best := ""
		for mk, n := range v {
			if best == "" || n > v[best] || (n == v[best] && mk < best) {
				best = mk
			}
		}
		x.alias[a] = best
	}
	x.aliasesOf = map[string][]string{}
	for a, mk := range x.alias {
		x.aliasesOf[mk] = append(x.aliasesOf[mk], a)
	}
	x.byModel = map[[2]string][]int{}
	for i, d := range x.rows {
		if d.MFG == "" || d.MDL == "" {
			continue // matched by name only
		}
		if k, ok := x.key(d.MFG, d.MDL); ok {
			x.byModel[k] = append(x.byModel[k], i)
		}
	}
}

// key is the {vendor, model key} both index rows and devices are matched on.
func (x *DriverIndex) key(mfg, mdl string) ([2]string, bool) {
	v := x.vendor(mfg)
	m := x.modelKey(v, mfg, mdl)
	return [2]string{v, m}, v != "" && m != ""
}

// Match returns the best driver for a device's MFG and MDL: the family that
// comes first in priority (families not in it last), then recommended, then
// the name. A non-empty family restricts the match to that family.
func (x *DriverIndex) Match(mfg, mdl string, priority []string, family string) (Driver, bool) {
	if x == nil {
		return Driver{}, false
	}
	k, ok := x.key(mfg, mdl)
	if !ok {
		return Driver{}, false
	}
	rank := func(f string) int {
		if i := slices.Index(priority, f); i >= 0 {
			return i
		}
		return len(priority)
	}
	var best *Driver
	for _, i := range x.byModel[k] {
		d := &x.rows[i]
		if family != "" && d.Family != family {
			continue
		}
		if best == nil || rank(d.Family) < rank(best.Family) ||
			(rank(d.Family) == rank(best.Family) && (d.Recommended && !best.Recommended ||
				d.Recommended == best.Recommended && d.Name < best.Name)) {
			best = d
		}
	}
	if best == nil {
		return Driver{}, false
	}
	return *best, true
}

// Lookup returns family's driver with that name.
func (x *DriverIndex) Lookup(family, name string) (Driver, bool) {
	if x == nil {
		return Driver{}, false
	}
	return find(x.rows, func(d Driver) bool { return d.Family == family && d.Name == name })
}

// families lists the index's families, sorted.
func (x *DriverIndex) families() []string {
	var out []string
	for _, d := range x.rows {
		if !slices.Contains(out, d.Family) {
			out = append(out, d.Family)
		}
	}
	slices.Sort(out)
	return out
}

// vendor resolves a manufacturer string to a make key: through the learned
// aliases, then through the aliases of its first word, else alnum(mfg).
func (x *DriverIndex) vendor(mfg string) string {
	a := alnum(mfg)
	if v, ok := x.alias[a]; ok {
		return v
	}
	if v, ok := x.alias[alnum(firstWord(mfg))]; ok {
		return v
	}
	return a
}

var seriesRe = regexp.MustCompile(`\s*\bseries$`)

// modelKey normalises a model name: lowercase, without a trailing word
// "series", alnum, and without the longest make prefix that leaves a rest.
func (x *DriverIndex) modelKey(vendor, mfg, mdl string) string {
	k := alnum(seriesRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(mdl)), ""))
	prefix := ""
	for _, p := range append([]string{alnum(mfg), alnum(firstWord(mfg)), vendor}, x.aliasesOf[vendor]...) {
		if p != "" && len(p) > len(prefix) && len(k) > len(p) && strings.HasPrefix(k, p) {
			prefix = p
		}
	}
	return k[len(prefix):]
}

// makeKey is alnum of a driver name's make, the part before the first "--".
func makeKey(name string) string {
	mk, _, _ := strings.Cut(name, "--")
	return alnum(mk)
}

// alnum lowercases s and keeps only letters and digits.
func alnum(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// SaneIndex is sane-usb.tsv: SANE backends by "vvvv:pppp", sorted and unique.
type SaneIndex map[string][]string

var hex4Re = regexp.MustCompile(`^[0-9a-f]{4}$`)

// ParseSaneIndex reads sane-usb.tsv.
func ParseSaneIndex(r io.Reader) (SaneIndex, error) {
	s := SaneIndex{}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if n == 1 {
			if line != saneIndexHeader {
				return nil, fmt.Errorf("sane index: line 1: want %q", saneIndexHeader)
			}
			continue
		}
		f := strings.Split(strings.ToLower(line), "\t")
		if len(f) != 3 || !hex4Re.MatchString(f[0]) || !hex4Re.MatchString(f[1]) || f[2] == "" {
			return nil, fmt.Errorf("sane index: line %d: want vvvv, pppp, backend", n)
		}
		id := f[0] + ":" + f[1]
		s[id] = append(s[id], f[2])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sane index: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("sane index: line 1: want %q", saneIndexHeader)
	}
	for id, b := range s {
		slices.Sort(b)
		s[id] = slices.Compact(b)
	}
	return s, nil
}
