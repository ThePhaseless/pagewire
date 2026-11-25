package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	"github.com/go-playground/validator/v10"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

var (
	hex4Re     = regexp.MustCompile(`^[0-9a-f]{4}$`)
	pwgMediaRe = regexp.MustCompile(`^[a-z0-9-]+_[a-z0-9.-]+_[0-9.]+x[0-9.]+(mm|in)$`)
	keywordRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)
	entryIDRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
)

var structValidator = func() *validator.Validate {
	v := validator.New()
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		return strings.Split(f.Tag.Get("json"), ",")[0]
	})
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(v.RegisterValidation("hex4", func(fl validator.FieldLevel) bool { return hex4Re.MatchString(fl.Field().String()) }))
	must(v.RegisterValidation("pwgmedia", func(fl validator.FieldLevel) bool { return pwgMediaRe.MatchString(fl.Field().String()) }))
	must(v.RegisterValidation("ippkeyword", func(fl validator.FieldLevel) bool { return keywordRe.MatchString(fl.Field().String()) }))
	must(v.RegisterValidation("entryid", func(fl validator.FieldLevel) bool { return entryIDRe.MatchString(fl.Field().String()) }))
	must(v.RegisterValidation("nocontrol", func(fl validator.FieldLevel) bool {
		return !strings.ContainsFunc(fl.Field().String(), unicode.IsControl)
	}))
	return v
}()

// Parse decodes a config file strictly (unknown keys are errors), fills in
// defaults for missing keys and runs the struct checks. Every failure is an
// *ErrorList whose entries carry the JSON pointer and the file position.
func Parse(b []byte) (*Config, error) {
	c, _, err := parse(b)
	return c, err
}

func parse(b []byte) (*Config, *ast.File, error) {
	// Decode onto the defaults so missing keys keep them, but leave the two
	// required keys empty so their absence is reported.
	c := Default("")
	c.APIVersion = ""
	file, err := decode(b, &c, func(body ast.Node) { fillDefaults(body, &c) })
	if err != nil {
		return nil, nil, err
	}
	return &c, file, nil
}

// decode parses b as one YAML document, decodes it strictly onto c (a pointer
// to a struct holding its defaults), calls fill with the document body and
// runs the struct checks.
func decode(b []byte, c any, fill func(body ast.Node)) (*ast.File, error) {
	file, err := parser.ParseBytes(b, 0)
	if err != nil {
		return nil, &ErrorList{Errors: []FieldError{yamlError(nil, err)}}
	}
	var body ast.Node
	if len(file.Docs) > 0 {
		body = file.Docs[0].Body
	}
	if len(file.Docs) > 1 {
		line, col := position(file.Docs[1])
		return nil, &ErrorList{Errors: []FieldError{{Line: line, Column: col, Message: "multiple YAML documents are not supported"}}}
	}
	if body != nil {
		literalStrings(body, reflect.TypeOf(c))
		if err := yaml.NodeToValue(body, c, yaml.Strict()); err != nil {
			return nil, &ErrorList{Errors: []FieldError{yamlError(body, err)}}
		}
	}
	fill(body)

	if err := structValidator.Struct(c); err != nil {
		var verrs validator.ValidationErrors
		if !errors.As(err, &verrs) {
			return nil, err
		}
		el := &ErrorList{}
		for _, fe := range verrs {
			el.Errors = append(el.Errors, FieldError{Path: namespacePointer(fe.Namespace()), Message: validationMessage(fe)})
		}
		locate(body, el)
		return nil, el
	}
	return file, nil
}

// fillDefaults fills what decoding onto Default cannot: the defaults inside
// list entries, and empty lists for null or missing ones. enabled is true
// when its key is absent or null.
func fillDefaults(body ast.Node, c *Config) {
	or := func(s *string, def string) {
		if *s == "" {
			*s = def
		}
	}
	for i := range c.Printers {
		p := &c.Printers[i]
		if !hasKey(body, fmt.Sprintf("/printers/%d", i), "enabled") {
			p.Enabled = true
		}
		or(&p.Mode, ModeUSB)
		or(&p.Driver, Auto)
	}
	for i := range c.Scanners {
		s := &c.Scanners[i]
		if !hasKey(body, fmt.Sprintf("/scanners/%d", i), "enabled") {
			s.Enabled = true
		}
		or(&s.Mode, ModeUSB)
		or(&s.Backend, BackendAuto)
	}
	for i := range c.IgnoredDevices {
		if c.IgnoredDevices[i].Roles == nil {
			c.IgnoredDevices[i].Roles = []string{}
		}
	}
	if c.Printers == nil {
		c.Printers = []Printer{}
	}
	if c.Scanners == nil {
		c.Scanners = []Scanner{}
	}
	if c.IgnoredDevices == nil {
		c.IgnoredDevices = []IgnoredDevice{}
	}
	if c.Licenses.AcceptedEULAs == nil {
		c.Licenses.AcceptedEULAs = []EULARef{}
	}
}

// hasKey reports whether the mapping at the JSON pointer has key with a
// non-null value.
//
// shortcut: only literal keys count, so an enabled key that reaches an entry
// through a merge key (<<: *base) is not seen; resolve merges if configs use them.
func hasKey(body ast.Node, pointer, key string) bool {
	n := body
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if n == nil {
			return false
		}
		n = child(unwrap(n), seg)
	}
	if n == nil {
		return false
	}
	v := child(unwrap(n), key)
	if v == nil {
		return false
	}
	_, null := unwrap(v).(*ast.NullNode)
	return !null
}

// literalStrings replaces every integer, float or bool scalar that sits at a
// string field with a string holding its literal text, so an unquoted
// "serial: 012345" stays "012345" instead of goccy's octal "5349".
func literalStrings(n ast.Node, t reflect.Type) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	fix := func(v ast.Node, vt reflect.Type) ast.Node {
		switch v.(type) {
		case *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode, *ast.InfinityNode, *ast.NanNode:
			if vt.Kind() == reflect.String {
				return ast.String(v.GetToken())
			}
		}
		literalStrings(v, vt)
		return v
	}
	n = unwrap(n)
	switch t.Kind() {
	case reflect.Struct:
		for _, mv := range entries(n) {
			if f, ok := fieldByJSONName(t, keyName(mv)); ok {
				mv.Value = fix(mv.Value, f.Type)
			}
		}
	case reflect.Slice:
		if seq, ok := n.(*ast.SequenceNode); ok {
			for i, v := range seq.Values {
				seq.Values[i] = fix(v, t.Elem())
			}
		}
	}
}

func fieldByJSONName(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		if f := t.Field(i); strings.Split(f.Tag.Get("json"), ",")[0] == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// yamlError turns a goccy error into a FieldError with its position and, when
// the token belongs to a node of body, that node's JSON pointer.
func yamlError(body ast.Node, err error) FieldError {
	var ye yaml.Error
	if !errors.As(err, &ye) {
		return FieldError{Message: err.Error()}
	}
	fe := FieldError{Message: ye.GetMessage()}
	var te *yaml.TypeError
	if errors.As(err, &te) {
		fe.Message = typeMessage(te.DstType)
	}
	if tk := ye.GetToken(); tk != nil && tk.Position != nil {
		fe.Line, fe.Column = tk.Position.Line, tk.Position.Column
		fe.Path = pointerOf(body, tk)
	}
	return fe
}

func typeMessage(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "must be an integer"
	case reflect.Bool:
		return "must be true or false"
	case reflect.String:
		return "must be a string"
	case reflect.Struct, reflect.Pointer, reflect.Map:
		return "must be a mapping"
	case reflect.Slice, reflect.Array:
		return "must be a list"
	}
	return "has the wrong type"
}

func validationMessage(fe validator.FieldError) string {
	isString := fe.Kind() == reflect.String
	switch fe.Tag() {
	case "required":
		return "is required"
	case "eq":
		return "must be " + fe.Param()
	case "oneof":
		return "must be one of: " + fe.Param()
	case "min":
		if isString {
			return "must be at least " + fe.Param() + " characters"
		}
		if fe.Kind() == reflect.Slice {
			return "must have at least " + fe.Param() + " entries"
		}
		return "must be at least " + fe.Param()
	case "max":
		if isString {
			return "must be at most " + fe.Param() + " characters"
		}
		return "must be at most " + fe.Param()
	case "hex4":
		return "must be 4 lowercase hex digits"
	case "pwgmedia":
		return "must be a PWG self-describing media name such as iso_a4_210x297mm"
	case "ippkeyword":
		return "must be a lower-case IPP keyword"
	case "entryid":
		return "must be 1 to 32 lower-case letters, digits or dashes, not starting or ending with a dash"
	case "nocontrol":
		return "must not contain control characters"
	case "startsnotwith":
		return "must not start with " + fe.Param()
	case "unique":
		return "has duplicate entries"
	}
	return "fails the " + fe.Tag() + " check"
}

// namespacePointer turns "Config.printers[0].defaults.media" into
// "/printers/0/defaults/media".
func namespacePointer(ns string) string {
	_, rest, _ := strings.Cut(ns, ".")
	rest = strings.NewReplacer("[", ".", "]", "").Replace(rest)
	return "/" + strings.ReplaceAll(rest, ".", "/")
}

// locate fills Line and Column of every error that has none from the node at
// its path, or from the nearest existing ancestor when the key is missing.
func locate(body ast.Node, el *ErrorList) {
	for i := range el.Errors {
		fe := &el.Errors[i]
		if fe.Line != 0 {
			continue
		}
		fe.Line, fe.Column = 1, 1
		if n := lookup(body, fe.Path); n != nil {
			if line, col := position(n); line > 0 {
				fe.Line, fe.Column = line, col
			}
		}
	}
}

// lookup returns the node at the JSON pointer, or its nearest existing ancestor.
func lookup(n ast.Node, pointer string) ast.Node {
	if n == nil || pointer == "" {
		return n
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		next := child(unwrap(n), seg)
		if next == nil {
			return n
		}
		n = next
	}
	return n
}

func child(n ast.Node, seg string) ast.Node {
	if seq, ok := n.(*ast.SequenceNode); ok {
		var i int
		if _, err := fmt.Sscanf(seg, "%d", &i); err == nil && i >= 0 && i < len(seq.Values) {
			return seq.Values[i]
		}
		return nil
	}
	for _, mv := range entries(n) {
		if keyName(mv) == seg {
			return mv.Value
		}
	}
	return nil
}

func unwrap(n ast.Node) ast.Node {
	for {
		switch v := n.(type) {
		case *ast.AnchorNode:
			n = v.Value
		case *ast.TagNode:
			n = v.Value
		default:
			return n
		}
	}
}

func entries(n ast.Node) []*ast.MappingValueNode {
	switch v := n.(type) {
	case *ast.MappingNode:
		return v.Values
	case *ast.MappingValueNode:
		return []*ast.MappingValueNode{v}
	}
	return nil
}

func keyName(mv *ast.MappingValueNode) string {
	if tk := mv.Key.GetToken(); tk != nil {
		return tk.Value
	}
	return ""
}

// position reports where a node starts: a mapping at its first key.
func position(n ast.Node) (line, col int) {
	switch v := n.(type) {
	case *ast.MappingNode:
		if len(v.Values) > 0 && !v.IsFlowStyle {
			return position(v.Values[0])
		}
	case *ast.MappingValueNode:
		return position(v.Key)
	case *ast.DocumentNode:
		if v.Start != nil {
			return v.Start.Position.Line, v.Start.Position.Column
		}
		if v.Body != nil {
			return position(v.Body)
		}
	}
	if tk := n.GetToken(); tk != nil && tk.Position != nil {
		return tk.Position.Line, tk.Position.Column
	}
	return 0, 0
}

// pointerOf returns the JSON pointer of the deepest node of body whose token
// sits at tk's position, or "" when there is none.
func pointerOf(body ast.Node, tk *token.Token) string {
	found := ""
	var walk func(n ast.Node, ptr string)
	walk = func(n ast.Node, ptr string) {
		if n == nil {
			return
		}
		if t := n.GetToken(); t != nil && t.Position != nil && t.Position.Line == tk.Position.Line && t.Position.Column == tk.Position.Column {
			found = ptr
		}
		n = unwrap(n)
		if seq, ok := n.(*ast.SequenceNode); ok {
			for i, v := range seq.Values {
				walk(v, fmt.Sprintf("%s/%d", ptr, i))
			}
			return
		}
		for _, mv := range entries(n) {
			p := ptr + "/" + keyName(mv)
			walk(mv.Key, p)
			walk(mv.Value, p)
		}
	}
	walk(body, "")
	return found
}
