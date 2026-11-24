// Package yamledit applies RFC 7396 JSON merge patches to YAML files while
// keeping every byte the patch does not touch: comments, blank lines, quoting
// and alignment elsewhere stay as they were.
//
// It locates nodes with the goccy AST and splices the source text at the byte
// offsets of the lexer tokens, so nothing outside the edited spans is
// re-encoded.
package yamledit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/lexer"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// ErrInvalidPatch means the patch body is not a JSON object.
var ErrInvalidPatch = errors.New("merge patch must be a JSON object")

// ErrUnsupported means the patch touches a YAML construct Apply does not edit.
//
// shortcut: flow mappings, anchors, aliases, tags and merge keys on a patched
// path are refused rather than edited; add support if hand-written configs
// start using them.
var ErrUnsupported = errors.New("unsupported YAML for merge patch")

// Apply applies mergePatch (RFC 7396) to the YAML document src. Values set by
// the patch are written in place, keeping the key's head and line comments and
// the quote style of a replaced string; new keys go at the end of their
// mapping; null deletes a key together with its comments. A patch that changes
// nothing returns src unchanged.
func Apply(src, mergePatch []byte) ([]byte, error) {
	p, err := decodePatch(mergePatch)
	if err != nil {
		return nil, err
	}
	d, err := load(src)
	if err != nil {
		return nil, err
	}
	if err := d.mergeRoot(p); err != nil {
		return nil, err
	}
	return d.splice()
}

// object is a JSON object that keeps the patch's key order, so the output is
// deterministic.
type object []member

type member struct {
	key string
	val any // nil, bool, string, json.Number, []any or object
}

func decodePatch(b []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPatch, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the object", ErrInvalidPatch)
	}
	o, ok := v.(object)
	if !ok {
		return nil, ErrInvalidPatch
	}
	return o, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		o := object{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			// A repeated key keeps its first position and its last value, as encoding/json does.
			if i := slices.IndexFunc(o, func(m member) bool { return m.key == k }); i >= 0 {
				o[i].val = v
			} else {
				o = append(o, member{k, v})
			}
		}
		_, err := dec.Token()
		return o, err
	case json.Delim('['):
		a := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		return a, err
	}
	return tok, nil
}

type edit struct {
	start, end int
	text       string
}

type document struct {
	src        []byte
	tokens     token.Tokens
	start, end []int // byte span of each token without surrounding whitespace
	idx        map[*token.Token]int
	body       ast.Node
	unit       int // mapping indent step
	seqIndent  int // column of "-" relative to its key
	edits      []edit
}

func load(src []byte) (*document, error) {
	d := &document{src: src, tokens: lexer.Tokenize(string(src)), idx: map[*token.Token]int{}}
	// Offsets come from the token origins, which concatenate to the source;
	// the parser rewrites origins, so they are read before parsing.
	// Position.Offset counts runes, not bytes, and is not used.
	off := 0
	for i, tk := range d.tokens {
		o := tk.Origin
		if off+len(o) > len(src) || string(src[off:off+len(o)]) != o {
			return nil, fmt.Errorf("%w: lexer tokens do not cover the source", ErrUnsupported)
		}
		s := off + len(o) - len(strings.TrimLeft(o, " \t\r\n"))
		e := off + len(strings.TrimRight(o, " \t\r\n"))
		d.start = append(d.start, s)
		d.end = append(d.end, max(s, e))
		d.idx[tk] = i
		off += len(o)
	}
	// Trailing whitespace at the end of the file belongs to no token.
	if len(bytes.TrimSpace(src[off:])) > 0 {
		return nil, fmt.Errorf("%w: lexer tokens do not cover the source", ErrUnsupported)
	}
	f, err := parser.Parse(d.tokens, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if len(f.Docs) > 1 {
		return nil, fmt.Errorf("%w: more than one document", ErrUnsupported)
	}
	if len(f.Docs) == 1 {
		d.body = f.Docs[0].Body
	}
	if _, ok := d.body.(*ast.CommentGroupNode); ok {
		d.body = nil // a file holding only comments
	}
	d.detectIndent()
	return d, nil
}

// detectIndent reads the file's mapping indent step and sequence indent from
// the first nested block mapping and block sequence; both default to 2.
func (d *document) detectIndent() {
	d.unit, d.seqIndent = 0, -1
	ast.Walk(visitor(func(n ast.Node) {
		mv, ok := n.(*ast.MappingValueNode)
		if !ok {
			return
		}
		kc, ok := d.col(mv.Key.GetToken())
		if !ok {
			return
		}
		switch v := mv.Value.(type) {
		case *ast.MappingNode:
			if v.IsFlowStyle || len(v.Values) == 0 || d.unit != 0 {
				return
			}
			if c, ok := d.col(v.Values[0].Key.GetToken()); ok && c > kc {
				d.unit = c - kc
			}
		case *ast.SequenceNode:
			if c, ok := d.col(v.Start); ok && !v.IsFlowStyle && d.seqIndent < 0 && c >= kc {
				d.seqIndent = c - kc
			}
		}
	}), d.bodyOrEmpty())
	if d.unit == 0 {
		d.unit = 2
	}
	if d.seqIndent < 0 {
		d.seqIndent = d.unit
	}
}

func (d *document) bodyOrEmpty() ast.Node {
	if d.body == nil {
		return ast.Mapping(nil, false)
	}
	return d.body
}

type visitor func(ast.Node)

func (v visitor) Visit(n ast.Node) ast.Visitor {
	if n != nil {
		v(n)
	}
	return v
}

func (d *document) mergeRoot(p object) error {
	if d.body == nil {
		return d.merge(nil, 0, len(d.src), p)
	}
	entries, ok := blockEntries(d.body)
	if !ok {
		return fmt.Errorf("%w: the document root is not a block mapping", ErrUnsupported)
	}
	indent, _ := d.col(entries[0].Key.GetToken())
	return d.merge(entries, indent, d.mappingEnd(entries, indent), p)
}

// merge records the edits that apply p to the mapping made of entries, whose
// keys sit at column indent and whose new keys are inserted at appendAt.
func (d *document) merge(entries []*ast.MappingValueNode, indent, appendAt int, p object) error {
	for _, mv := range entries {
		if _, ok := mv.Key.(*ast.MergeKeyNode); ok {
			return fmt.Errorf("%w: merge key in a patched mapping", ErrUnsupported)
		}
	}
	var appends object
	for _, m := range p {
		mv := find(entries, m.key)
		switch {
		case m.val == nil:
			if mv != nil {
				if err := d.delete(mv); err != nil {
					return err
				}
			}
		case mv == nil:
			appends = append(appends, member{m.key, stripNulls(m.val)})
		default:
			po, isObj := m.val.(object)
			if !isObj {
				if d.equal(mv.Value, m.val) {
					continue
				}
				if err := d.replace(mv, m.val); err != nil {
					return err
				}
				continue
			}
			if child, ok := blockEntries(mv.Value); ok {
				if emptiedBy(child, po) {
					if err := d.replace(mv, object{}); err != nil {
						return err
					}
					continue
				}
				ci, _ := d.col(child[0].Key.GetToken())
				if err := d.merge(child, ci, d.mappingEnd(child, ci), po); err != nil {
					return err
				}
				continue
			}
			switch v := mv.Value.(type) {
			case *ast.AnchorNode, *ast.AliasNode, *ast.TagNode:
				return fmt.Errorf("%w: anchor, alias or tag at %q", ErrUnsupported, m.key)
			case *ast.MappingNode:
				if len(v.Values) > 0 {
					return fmt.Errorf("%w: flow mapping at %q", ErrUnsupported, m.key)
				}
			}
			// Merging into a non-object starts from {}, which leaves the patch without its nulls.
			if err := d.replace(mv, stripNulls(po)); err != nil {
				return err
			}
		}
	}
	if len(appends) == 0 {
		return nil
	}
	lines := d.renderMapping(appends, indent)
	text := strings.Join(lines, "\n") + "\n"
	if appendAt == len(d.src) && len(d.src) > 0 && d.src[len(d.src)-1] != '\n' {
		text = "\n" + text
	}
	d.edits = append(d.edits, edit{appendAt, appendAt, text})
	return nil
}

func blockEntries(n ast.Node) ([]*ast.MappingValueNode, bool) {
	switch v := n.(type) {
	case *ast.MappingNode:
		return v.Values, !v.IsFlowStyle && len(v.Values) > 0
	case *ast.MappingValueNode:
		return []*ast.MappingValueNode{v}, !v.IsFlowStyle
	}
	return nil, false
}

func find(entries []*ast.MappingValueNode, key string) *ast.MappingValueNode {
	for _, mv := range entries {
		if tk := mv.Key.GetToken(); tk != nil && tk.Value == key {
			return mv
		}
	}
	return nil
}

// emptiedBy reports whether p deletes every key of the mapping and adds none.
func emptiedBy(entries []*ast.MappingValueNode, p object) bool {
	left := len(entries)
	for _, m := range p {
		exists := find(entries, m.key) != nil
		switch {
		case m.val == nil && exists:
			left--
		case m.val != nil && !exists:
			left++
		}
	}
	return left == 0
}

func stripNulls(v any) any {
	o, ok := v.(object)
	if !ok {
		return v
	}
	out := object{}
	for _, m := range o {
		if m.val != nil {
			out = append(out, member{m.key, stripNulls(m.val)})
		}
	}
	return out
}

// equal reports whether node already holds v, so a no-op patch makes no edit.
func (d *document) equal(node ast.Node, v any) bool {
	var cur any
	if err := yaml.NodeToValue(node, &cur); err != nil {
		return false
	}
	a, okA := canonical(cur)
	b, okB := canonical(plain(v))
	return okA && okB && bytes.Equal(a, b)
}

// canonical is v as JSON with numbers normalised and map keys sorted.
func canonical(v any) ([]byte, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var x any
	if json.Unmarshal(b, &x) != nil {
		return nil, false
	}
	b, err = json.Marshal(x)
	return b, err == nil
}

func plain(v any) any {
	switch t := v.(type) {
	case object:
		m := make(map[string]any, len(t))
		for _, e := range t {
			m[e.key] = plain(e.val)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plain(e)
		}
		return out
	}
	return v
}

// delete removes a key line with its head comments (comment lines directly
// above at the key's indent), its value and the comments indented under it.
// When that leaves two blank lines in a row, the one above goes too.
func (d *document) delete(mv *ast.MappingValueNode) error {
	if err := d.refuseAnchors(mv.Value); err != nil {
		return err
	}
	ki, ok := d.idx[mv.Key.GetToken()]
	if !ok {
		return fmt.Errorf("%w: key without a source token", ErrUnsupported)
	}
	start := d.lineStart(d.start[ki])
	if strings.TrimSpace(string(d.src[start:d.start[ki]])) != "" {
		return fmt.Errorf("%w: key %q does not start its line", ErrUnsupported, mv.Key.GetToken().Value)
	}
	keyCol := d.start[ki] - start
	for start > 0 {
		prev := d.lineStart(start - 1)
		// A comment indented differently belongs to a neighbour, not to this key.
		if !isComment(d.src[prev:start]) || indentOf(d.src[prev:start]) != keyCol {
			break
		}
		start = prev
	}
	end := d.nextLine(d.lastEnd(mv))
	for end < len(d.src) {
		next := d.nextLine(end)
		if !isComment(d.src[end:next]) || indentOf(d.src[end:next]) <= keyCol {
			break
		}
		end = next
	}
	if start > 0 {
		prev := d.lineStart(start - 1)
		if isBlank(d.src[prev:start]) && (end == len(d.src) || isBlank(d.src[end:d.nextLine(end)])) {
			start = prev
		}
	}
	d.edits = append(d.edits, edit{start, end, ""})
	return nil
}

// replace swaps the value of mv for v, keeping the key and its line comment,
// whose column is kept when the new value fits before it.
func (d *document) replace(mv *ast.MappingValueNode, v any) error {
	if err := d.refuseAnchors(mv.Value); err != nil {
		return err
	}
	colon, ok := d.idx[mv.Start]
	if !ok {
		return fmt.Errorf("%w: mapping value without a source token", ErrUnsupported)
	}
	colonEnd := d.end[colon]
	first, last := d.span(mv.Value)
	regionEnd := colonEnd
	after := colon + 1 // the token that may hold the key line's comment
	if first >= 0 {
		regionEnd = d.end[last]
		if d.lineStart(d.start[first]) == d.lineStart(colonEnd) {
			after = last + 1
		}
	}
	var comment string
	commentCol := 0
	if after < len(d.tokens) && d.tokens[after].Type == token.CommentType && d.lineStart(d.start[after]) == d.lineStart(colonEnd) {
		comment = string(d.src[d.start[after]:d.end[after]])
		commentCol = d.runeCol(d.start[after])
		regionEnd = max(regionEnd, d.end[after])
	}

	keyCol := d.runeCol(d.start[d.idx[mv.Key.GetToken()]])
	var inline string
	var block []string
	switch t := v.(type) {
	case object:
		if len(t) == 0 {
			inline = " {}"
		} else {
			block = d.renderMapping(t, keyCol+d.unit)
		}
	case []any:
		if len(t) == 0 {
			inline = " []"
		} else {
			block = d.renderSequence(t, keyCol+d.seqIndent)
		}
	default:
		inline = " " + scalar(v, styleOf(mv.Value))
	}
	text := inline
	if comment != "" {
		pad := commentCol - d.runeCol(colonEnd) - utf8.RuneCountInString(inline)
		text += strings.Repeat(" ", max(1, pad)) + comment
	}
	if len(block) > 0 {
		text += "\n" + strings.Join(block, "\n")
	}
	d.edits = append(d.edits, edit{colonEnd, regionEnd, text})
	return nil
}

func (d *document) refuseAnchors(n ast.Node) error {
	var found bool
	ast.Walk(visitor(func(n ast.Node) {
		if _, ok := n.(*ast.AnchorNode); ok {
			found = true
		}
	}), n)
	if found {
		return fmt.Errorf("%w: the value carries an anchor", ErrUnsupported)
	}
	return nil
}

// span returns the first and last non-comment token of n, or -1, -1 when n has
// none in the source (an implicit null).
func (d *document) span(n ast.Node) (first, last int) {
	first, last = -1, -1
	add := func(tk *token.Token) {
		i, ok := d.idx[tk]
		if !ok || tk.Type == token.CommentType {
			return
		}
		if first < 0 || i < first {
			first = i
		}
		last = max(last, i)
	}
	ast.Walk(visitor(func(n ast.Node) {
		switch v := n.(type) {
		case *ast.CommentNode, *ast.CommentGroupNode:
			return
		case *ast.MappingNode:
			add(v.End)
		case *ast.SequenceNode:
			add(v.End)
		}
		add(n.GetToken())
	}), n)
	return first, last
}

// lastEnd is the byte offset just past the last token of an entry.
func (d *document) lastEnd(mv *ast.MappingValueNode) int {
	_, last := d.span(mv)
	return d.end[last]
}

// mappingEnd is where a new key of a mapping goes: the start of the line after
// its last entry and after any comment lines that directly follow at or below
// the mapping's indent.
func (d *document) mappingEnd(entries []*ast.MappingValueNode, indent int) int {
	end := d.nextLine(d.lastEnd(entries[len(entries)-1]))
	for end < len(d.src) {
		next := d.nextLine(end)
		if !isComment(d.src[end:next]) || indentOf(d.src[end:next]) < indent {
			break
		}
		end = next
	}
	return end
}

func (d *document) col(tk *token.Token) (int, bool) {
	i, ok := d.idx[tk]
	if !ok {
		return 0, false
	}
	return d.start[i] - d.lineStart(d.start[i]), true
}

func (d *document) runeCol(off int) int {
	return utf8.RuneCount(d.src[d.lineStart(off):off])
}

func (d *document) lineStart(off int) int {
	return bytes.LastIndexByte(d.src[:off], '\n') + 1
}

// nextLine returns the start of the line after the one holding off.
func (d *document) nextLine(off int) int {
	if i := bytes.IndexByte(d.src[off:], '\n'); i >= 0 {
		return off + i + 1
	}
	return len(d.src)
}

func isComment(line []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte("#"))
}

func isBlank(line []byte) bool {
	return len(bytes.TrimSpace(line)) == 0
}

func indentOf(line []byte) int {
	return len(line) - len(bytes.TrimLeft(line, " "))
}

// splice applies the edits to the source in one pass. Inserts at the same
// offset keep the order they were recorded in (inner mappings first) and come
// before a deletion starting there.
func (d *document) splice() ([]byte, error) {
	if len(d.edits) == 0 {
		return d.src, nil
	}
	slices.SortStableFunc(d.edits, func(a, b edit) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return (a.end - a.start) - (b.end - b.start)
	})
	var out bytes.Buffer
	pos := 0
	for _, e := range d.edits {
		if e.start < pos {
			return nil, errors.New("yamledit: overlapping edits")
		}
		out.Write(d.src[pos:e.start])
		out.WriteString(e.text)
		pos = e.end
	}
	out.Write(d.src[pos:])
	return out.Bytes(), nil
}

func styleOf(n ast.Node) token.Type {
	if s, ok := n.(*ast.StringNode); ok && s.Token != nil {
		return s.Token.Type
	}
	return token.StringType
}

func isBlock(v any) bool {
	switch t := v.(type) {
	case object:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return false
}

func (d *document) renderMapping(o object, indent int) []string {
	var lines []string
	pad := strings.Repeat(" ", indent)
	for _, m := range o {
		key := scalar(m.key, token.StringType)
		switch t := m.val.(type) {
		case object:
			if isBlock(t) {
				lines = append(lines, pad+key+":")
				lines = append(lines, d.renderMapping(t, indent+d.unit)...)
				continue
			}
		case []any:
			if isBlock(t) {
				lines = append(lines, pad+key+":")
				lines = append(lines, d.renderSequence(t, indent+d.seqIndent)...)
				continue
			}
		}
		lines = append(lines, pad+key+": "+inlineValue(m.val))
	}
	return lines
}

func (d *document) renderSequence(a []any, indent int) []string {
	var lines []string
	pad := strings.Repeat(" ", indent)
	for _, e := range a {
		var sub []string
		switch t := e.(type) {
		case object:
			if isBlock(t) {
				sub = d.renderMapping(t, indent+2)
			}
		case []any:
			if isBlock(t) {
				sub = d.renderSequence(t, indent+2)
			}
		}
		if sub == nil {
			lines = append(lines, pad+"- "+inlineValue(e))
			continue
		}
		sub[0] = pad + "- " + sub[0][indent+2:]
		lines = append(lines, sub...)
	}
	return lines
}

func inlineValue(v any) string {
	switch t := v.(type) {
	case object:
		if len(t) == 0 {
			return "{}"
		}
	case []any:
		if len(t) == 0 {
			return "[]"
		}
	}
	return scalar(v, token.StringType)
}

// scalar renders a JSON scalar. A string keeps a single- or double-quoted
// style; otherwise it is plain when it reads back as the same string and
// does not look numeric, and double-quoted when not.
func scalar(v any, style token.Type) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case string:
		switch {
		case style == token.SingleQuoteType && !strings.ContainsAny(t, "\n\r"):
			return "'" + strings.ReplaceAll(t, "'", "''") + "'"
		case style == token.DoubleQuoteType || !plainOK(t):
			return strconv.Quote(t)
		}
		return t
	}
	return fmt.Sprint(v)
}

func plainOK(s string) bool {
	if s == "" || strings.ContainsAny(s, "\n\r\t") || strings.ContainsAny(s[:1], "0123456789+-.") {
		return false
	}
	var asValue map[string]any
	if yaml.Unmarshal([]byte("k: "+s), &asValue) != nil || asValue["k"] != s || len(asValue) != 1 {
		return false
	}
	var asKey map[string]any
	if yaml.Unmarshal([]byte(s+": v"), &asKey) != nil || len(asKey) != 1 {
		return false
	}
	_, ok := asKey[s]
	return ok
}
