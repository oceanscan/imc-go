package dunetask

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// ParamType lists the Go types of parameters: DUNE's bool, integer, real
// and string parameters, and lists of them.
type ParamType interface {
	bool | int | int64 | float64 | string | []bool | []int | []int64 | []float64 | []string
}

// ParamError is an invalid parameter value or an unknown parameter.
type ParamError struct{ Msg string }

// Error describes the error.
func (e *ParamError) Error() string { return e.Msg }

func paramErr(format string, args ...any) error { return &ParamError{fmt.Sprintf(format, args...)} }

// ParamOption sets an attribute of a parameter.
type ParamOption func(*paramDecl)

// Units of the parameter (shown by Neptus).
func Units(u string) ParamOption { return func(p *paramDecl) { p.units = u } }

// Desc is the description of the parameter.
func Desc(d string) ParamOption { return func(p *paramDecl) { p.desc = d } }

// Min is the minimum of a numeric parameter (of each item of a list).
func Min(v float64) ParamOption { return func(p *paramDecl) { p.min = &v } }

// Max is the maximum of a numeric parameter (of each item of a list).
func Max(v float64) ParamOption { return func(p *paramDecl) { p.max = &v } }

// Values restricts the parameter (each item of a list) to these values,
// written as DUNE text (e.g. "1.5", "true", "a").
func Values(values ...string) ParamOption {
	return func(p *paramDecl) { p.values = append([]string(nil), values...) }
}

// Visibility is "user" (default) or "developer".
func Visibility(v string) ParamOption { return func(p *paramDecl) { p.visibility = v } }

// Scope is "global" (default), "idle", "plan" or "maneuver"; only scopes
// other than global are saved by PushEntityParameters, as in DUNE.
func Scope(s string) ParamOption { return func(p *paramDecl) { p.scope = s } }

// paramDecl is the type independent part of a parameter.
type paramDecl struct {
	name       string
	units      string
	desc       string
	min, max   *float64
	values     []string
	visibility string
	scope      string
	// parse parses and validates text; set stores a parsed value; format
	// formats the current value; equal compares a parsed value with it.
	parse  func(string) (any, error)
	store  func(any)
	equal  func(any) bool
	format func() string
}

// Param is a typed parameter of a task. Get and Set are safe from any
// goroutine.
type Param[T ParamType] struct {
	decl  *paramDecl
	task  *Task
	value T
}

// NewParam declares a parameter of t. name is its DUNE name (e.g. "Maximum
// Depth"); def its default. It panics if the default is invalid or the name
// is taken (declarations are program errors).
func NewParam[T ParamType](t *Task, name string, def T, opts ...ParamOption) *Param[T] {
	d := &paramDecl{name: name, visibility: "user", scope: "global"}
	p := &Param[T]{task: t, decl: d}
	for _, o := range opts {
		o(d)
	}
	d.parse = func(text string) (any, error) {
		v, err := parseParam[T](text)
		if err != nil {
			return nil, paramErr("%s: %v", name, err)
		}
		if err := d.validate(v); err != nil {
			return nil, err
		}
		return v, nil
	}
	d.store = func(v any) { p.value = v.(T) }
	d.equal = func(v any) bool { return equalValues(p.value, v.(T)) }
	d.format = func() string { return formatParam(p.value) }
	if err := d.validate(def); err != nil {
		panic(fmt.Sprintf("dunetask: parameter %q: default: %v", name, err))
	}
	p.value = def
	t.params.add(d)
	return p
}

// Name returns the DUNE name of the parameter.
func (p *Param[T]) Name() string { return p.decl.name }

// Get returns the current value.
func (p *Param[T]) Get() T {
	p.task.params.mu.Lock()
	defer p.task.params.mu.Unlock()
	return p.value
}

// Set sets the value (validated).
func (p *Param[T]) Set(v T) error {
	ps := &p.task.params
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if err := p.decl.validate(v); err != nil {
		return err
	}
	p.value = v
	ps.strings[p.decl.name] = formatParam(v)
	return nil
}

func (d *paramDecl) validate(v any) error {
	var items []any
	switch x := v.(type) {
	case []bool:
		for _, i := range x {
			items = append(items, i)
		}
	case []int:
		for _, i := range x {
			items = append(items, i)
		}
	case []int64:
		for _, i := range x {
			items = append(items, i)
		}
	case []float64:
		for _, i := range x {
			items = append(items, i)
		}
	case []string:
		for _, i := range x {
			items = append(items, i)
		}
	default:
		items = []any{v}
	}
	for _, item := range items {
		var f float64
		numeric := true
		switch x := item.(type) {
		case int:
			f = float64(x)
		case int64:
			f = float64(x)
		case float64:
			f = x
		default:
			numeric = false
		}
		if numeric && d.min != nil && f < *d.min {
			return paramErr("%s: value is below minimum", d.name)
		}
		if numeric && d.max != nil && f > *d.max {
			return paramErr("%s: value is above maximum", d.name)
		}
		if d.values != nil {
			s := formatOne(item)
			ok := false
			for _, allowed := range d.values {
				if allowed == s {
					ok = true
				}
			}
			if !ok {
				return paramErr("%s: value not in values set", d.name)
			}
		}
	}
	return nil
}

// -- the parameters of a task ----------------------------------------------------

type params struct {
	mu      sync.Mutex
	decls   []*paramDecl
	byName  map[string]*paramDecl
	strings map[string]string // DUNE text of each value
	stack   []map[string]string
}

func (ps *params) add(d *paramDecl) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.byName == nil {
		ps.byName = map[string]*paramDecl{}
		ps.strings = map[string]string{}
	}
	if _, ok := ps.byName[d.name]; ok {
		panic(fmt.Sprintf("dunetask: parameter %q declared twice", d.name))
	}
	ps.decls = append(ps.decls, d)
	ps.byName[d.name] = d
	ps.strings[d.name] = d.format()
}

// setString sets a parameter from DUNE text. @return whether the value
// changed.
func (ps *params) setString(name, text string) (bool, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	d := ps.byName[name]
	if d == nil {
		return false, paramErr("unknown parameter %q", name)
	}
	v, err := d.parse(text)
	if err != nil {
		return false, err
	}
	changed := !d.equal(v)
	d.store(v)
	ps.strings[name] = text
	return changed, nil
}

// names returns the parameter names, sorted.
func (ps *params) names() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]string, 0, len(ps.decls))
	for _, d := range ps.decls {
		out = append(out, d.name)
	}
	sort.Strings(out)
	return out
}

func (ps *params) text(name string) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.strings[name]
}

func (ps *params) has(name string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.byName[name] != nil
}

func (ps *params) push() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	saved := map[string]string{}
	for _, d := range ps.decls {
		if d.scope != "global" {
			saved[d.name] = ps.strings[d.name]
		}
	}
	ps.stack = append(ps.stack, saved)
}

func (ps *params) pop() (map[string]string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if len(ps.stack) == 0 {
		return nil, false
	}
	top := ps.stack[len(ps.stack)-1]
	ps.stack = ps.stack[:len(ps.stack)-1]
	return top, true
}

// -- DUNE's parameter syntax ------------------------------------------------------

func parseBool(s string) (bool, error) {
	switch s {
	case "true", "True", "1":
		return true, nil
	case "false", "False", "0":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %q", s)
}

// parseInt parses like strtol with base 0 (0x hexadecimal, leading 0 octal).
func parseInt(s string) (int64, error) {
	t := strings.TrimSpace(s)
	v, err := strconv.ParseInt(t, 0, 64)
	if err != nil {
		// Python's int(x, 0) accepts '_' separators; strconv does too with
		// base 0. Anything else is an error.
		return 0, fmt.Errorf("not an integer: %q", s)
	}
	return v, nil
}

func parseFloat(s string) (float64, error) {
	t := strings.TrimSpace(s)
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		switch strings.ToLower(t) {
		case "nan", "+nan", "-nan":
			return math.NaN(), nil
		case "inf", "+inf", "infinity", "+infinity":
			return math.Inf(1), nil
		case "-inf", "-infinity":
			return math.Inf(-1), nil
		}
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return v, nil
		}
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return v, nil
}

func splitList(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	parts := strings.Split(text, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseParam[T ParamType](text string) (T, error) {
	var zero T
	var out any
	var err error
	switch any(zero).(type) {
	case bool:
		out, err = parseBool(strings.TrimSpace(text))
	case int:
		var v int64
		v, err = parseInt(text)
		if err == nil && (v < math.MinInt || v > math.MaxInt) {
			err = fmt.Errorf("not an integer: %q", text)
		}
		out = int(v)
	case int64:
		out, err = parseInt(text)
	case float64:
		out, err = parseFloat(text)
	case string:
		out = text
	case []bool:
		list := []bool{}
		for _, p := range splitList(text) {
			var v bool
			if v, err = parseBool(p); err != nil {
				break
			}
			list = append(list, v)
		}
		out = list
	case []int:
		list := []int{}
		for _, p := range splitList(text) {
			var v int64
			if v, err = parseInt(p); err != nil {
				break
			}
			list = append(list, int(v))
		}
		out = list
	case []int64:
		list := []int64{}
		for _, p := range splitList(text) {
			var v int64
			if v, err = parseInt(p); err != nil {
				break
			}
			list = append(list, v)
		}
		out = list
	case []float64:
		list := []float64{}
		for _, p := range splitList(text) {
			var v float64
			if v, err = parseFloat(p); err != nil {
				break
			}
			list = append(list, v)
		}
		out = list
	case []string:
		list := []string{}
		for _, p := range splitList(text) {
			list = append(list, p)
		}
		out = list
	}
	if err != nil {
		return zero, err
	}
	return out.(T), nil
}

// FormatFloat formats a real as Python's repr does (and so as the Python SDK
// reports parameters): the shortest text that reads back to the same value,
// with ".0" for integral values ("1.0", "0.1", "1e+16", "1e-05").
func FormatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(v, 'e', -1, 64) // d.ddde±XX
	mant, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	if exp < -4 || exp >= 16 {
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		return fmt.Sprintf("%se%s%02d", mant, sign, exp)
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

func formatOne(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return FormatFloat(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func formatParam(v any) string {
	var items []string
	switch x := v.(type) {
	case []bool:
		for _, i := range x {
			items = append(items, formatOne(i))
		}
	case []int:
		for _, i := range x {
			items = append(items, formatOne(i))
		}
	case []int64:
		for _, i := range x {
			items = append(items, formatOne(i))
		}
	case []float64:
		for _, i := range x {
			items = append(items, formatOne(i))
		}
	case []string:
		items = x
	default:
		return formatOne(v)
	}
	return strings.Join(items, ", ")
}

func equalValues(a, b any) bool {
	switch x := a.(type) {
	case []bool:
		y := b.([]bool)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case []int:
		y := b.([]int)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case []int64:
		y := b.([]int64)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case []float64:
		y := b.([]float64)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case []string:
		y := b.([]string)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return a == b
}

// LabelOf turns an identifier into DUNE's parameter-name style:
// "max_speed" or "maxSpeed" -> "Max Speed".
func LabelOf(ident string) string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(unicode.ToUpper(cur[0]))+string(cur[1:]))
			cur = nil
		}
	}
	for i, r := range ident {
		if r == '_' || r == ' ' {
			flush()
			continue
		}
		if unicode.IsUpper(r) && i > 0 {
			flush()
		}
		cur = append(cur, r)
	}
	flush()
	return strings.Join(words, " ")
}
