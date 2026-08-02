// Package config owns layered configuration resolution: struct defaults,
// then file sources, then environment variables, then command-line flags —
// later layers win, and the merged result is checked before boot continues.
//
// Core parses no files. Anything that needs a parser implements Source from
// its own submodule (config/yaml first); core only merges the maps sources
// return. A missing required value is a startup failure naming the field
// path and the exact variable, key, or flag that would set it — never a
// nil-pointer panic on the first query.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/MerseniBilel/warren"
)

// Source is where config values come from. Defaults, env, and flags ship in
// core; anything needing a parser implements Source from its own submodule.
// A Source is itself an Option, so it slots straight into Load. The returned
// map nests with map[string]any values; keys are the config: tag names.
type Source interface {
	Load() (map[string]any, error)
}

// Option configures loading: a Source value, WithEnvPrefix, or WithFlags.
// A Source is itself an Option — pass yaml.File("config.yaml") directly.
// Anything else fails Load with an error naming the offending type, so a
// mistake surfaces at boot, not silently.
type Option any

type settings struct {
	sources   []Source
	envPrefix string
	useEnv    bool
	flags     *flag.FlagSet
}

type optionFunc func(*settings)

// WithEnvPrefix enables the environment layer and sets its prefix: prefix
// "WARREN" maps the field path postgres.dsn to WARREN_POSTGRES_DSN. Without
// this option the environment is not consulted; an empty prefix is a boot
// error, because bare names collide with PATH, HOME, and friends.
//
// A variable that is set but empty counts as set — emptiness is a value, and
// required checks presence, not non-emptiness.
func WithEnvPrefix(prefix string) Option {
	return optionFunc(func(s *settings) { s.envPrefix = prefix; s.useEnv = true })
}

// WithFlags takes the final layer from fs, which must already be parsed —
// parsing arguments is main's business. Flag names are the dotted field
// path (-postgres.dsn), and only flags explicitly set on the command line
// override earlier layers; flag defaults do not participate.
func WithFlags(fs *flag.FlagSet) Option {
	return optionFunc(func(s *settings) { s.flags = fs })
}

// Module returns a warren.Module that loads T at boot — one provider over
// Load, resolved at instantiation in dependency order, so T is injectable by
// any constructor and a bad config fails the boot, never request 1. The
// module is named after T ("config[main.Config]"), so an application may
// load several config structs without the modules colliding. When main
// itself needs config values at composition time (adapter options), call
// Load once and provide the value instead — see warren.md §10.
func Module[T any](opts ...Option) warren.Module {
	return warren.NewModule("config["+reflect.TypeFor[T]().String()+"]",
		warren.Providers(func() (T, error) { return Load[T](opts...) }),
		warren.Eager[T](),
		warren.Exports[T](),
	)
}

// Load resolves configuration into T: struct defaults, then each Source in
// the order given, then environment variables, then command-line flags —
// later layers win, field by field. It checks the merged result before
// returning: every field tagged validate:"required" must have been set by
// some layer. T must be a struct whose participating fields carry config:
// tags.
func Load[T any](opts ...Option) (T, error) {
	var out T

	s := settings{}
	for _, opt := range opts {
		switch o := opt.(type) {
		case Source:
			s.sources = append(s.sources, o)
		case optionFunc:
			o(&s)
		default:
			return out, fmt.Errorf("config: %T is not an option — pass a Source, WithEnvPrefix, or WithFlags", opt)
		}
	}
	if s.flags != nil && !s.flags.Parsed() {
		return out, errors.New("config: the flag set given to WithFlags has not been parsed — call its Parse method before Load")
	}
	if s.useEnv && s.envPrefix == "" {
		return out, errors.New(`config: WithEnvPrefix("") would read bare variables like PATH — pass a real prefix`)
	}

	t := reflect.TypeOf(out)
	if t == nil || t.Kind() != reflect.Struct {
		return out, fmt.Errorf("config: T must be a struct, got %v", t)
	}
	fields, err := collectFields(t, nil, nil, nil)
	if err != nil {
		return out, err
	}
	byKey := map[string]string{}
	for _, f := range fields {
		if other, ok := byKey[f.key()]; ok {
			return out, fmt.Errorf("config: fields %s and %s both bind the key %q — config: tags must be unique", other, f.goPath, f.key())
		}
		byKey[f.key()] = f.goPath
	}

	// One entry per field path: the value and which layer supplied it.
	// Layers are applied in resolution order, so a later write is a later
	// layer winning.
	type entry struct {
		value  any
		origin string
	}
	entries := map[string]entry{}

	// Layer 1 — struct defaults.
	for _, f := range fields {
		if f.hasDefault {
			entries[f.key()] = entry{value: f.def, origin: "default"}
		}
	}

	// Layer 2 — sources, in the order given.
	for i, src := range s.sources {
		m, err := src.Load()
		if err != nil {
			return out, fmt.Errorf("config: source %d (%T) failed: %w", i+1, src, err)
		}
		for _, f := range fields {
			if v, ok := lookup(m, f.path); ok {
				entries[f.key()] = entry{value: v, origin: fmt.Sprintf("source %d (%T)", i+1, src)}
			}
		}
	}

	// Layer 3 — environment, only under an explicit prefix.
	if s.useEnv {
		for _, f := range fields {
			name := f.envName(s.envPrefix)
			if v, ok := os.LookupEnv(name); ok {
				entries[f.key()] = entry{value: v, origin: "environment variable " + name}
			}
		}
	}

	// Layer 4 — flags explicitly set on the command line.
	if s.flags != nil {
		set := map[string]string{}
		s.flags.Visit(func(fl *flag.Flag) { set[fl.Name] = fl.Value.String() })
		for _, f := range fields {
			if v, ok := set[f.key()]; ok {
				entries[f.key()] = entry{value: v, origin: "flag -" + f.key()}
			}
		}
	}

	// Bind the merged result and collect every failure, so one boot names
	// them all.
	v := reflect.ValueOf(&out).Elem()
	var errs []error
	for _, f := range fields {
		e, ok := entries[f.key()]
		if !ok {
			if f.required {
				errs = append(errs, errRequired(f, s))
			}
			continue
		}
		converted, err := convert(e.value, f.fieldType)
		if err != nil {
			var h hint
			switch {
			case e.origin == "default":
				errs = append(errs, fmt.Errorf("config: %s: default: tag %s is not a valid %s — fix the struct tag", f.key(), quote(e.value), f.fieldType))
			case errors.As(err, &h):
				errs = append(errs, fmt.Errorf("config: %s: cannot use %s (from %s) as %s — %s", f.key(), quote(e.value), e.origin, f.fieldType, h))
			default:
				errs = append(errs, fmt.Errorf("config: %s: cannot use %s (from %s) as %s", f.key(), quote(e.value), e.origin, f.fieldType))
			}
			continue
		}
		v.FieldByIndex(f.index).Set(converted)
	}
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// field is one participating leaf of the config struct.
type field struct {
	path       []string // config: tag names, outermost first
	goPath     string   // Go field names, "Postgres.DSN" — for tag errors
	index      []int    // reflect field index path
	fieldType  reflect.Type
	required   bool
	def        string
	hasDefault bool
}

// hint is a conversion failure that knows how to be fixed; Load appends it
// to the field/layer message.
type hint string

func (h hint) Error() string { return string(h) }

func (f field) key() string { return strings.Join(f.path, ".") }

func (f field) envName(prefix string) string {
	name := strings.ToUpper(strings.Join(f.path, "_"))
	if prefix == "" {
		return name
	}
	return prefix + "_" + name
}

var durationType = reflect.TypeFor[time.Duration]()

func collectFields(t reflect.Type, path, goPath []string, index []int) ([]field, error) {
	var out []field
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		key := sf.Tag.Get("config")
		if key == "" {
			continue
		}
		fpath := append(append([]string{}, path...), key)
		gpath := append(append([]string{}, goPath...), sf.Name)
		findex := append(append([]int{}, index...), i)

		if sf.Type.Kind() == reflect.Struct && sf.Type != durationType {
			// A required section is not enforceable as stated — silently
			// dropping the tag is the failure mode this package exists to
			// prevent, so reject the placement at boot.
			if hasValidateToken(sf.Tag.Get("validate"), "required") {
				return nil, fmt.Errorf("config: %s: validate:%q on a struct section does nothing — mark its leaf fields required instead", strings.Join(fpath, "."), "required")
			}
			nested, err := collectFields(sf.Type, fpath, gpath, findex)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			continue
		}

		def, hasDefault := sf.Tag.Lookup("default")
		out = append(out, field{
			path:       fpath,
			goPath:     strings.Join(gpath, "."),
			index:      findex,
			fieldType:  sf.Type,
			required:   hasValidateToken(sf.Tag.Get("validate"), "required"),
			def:        def,
			hasDefault: hasDefault,
		})
	}
	return out, nil
}

// hasValidateToken reports whether a validate: tag contains the exact token.
// Core enforces only "required" — completeness of resolution, not a
// validator; the rest of the validate: vocabulary is warren/validate's.
func hasValidateToken(tag, token string) bool {
	for part := range strings.SplitSeq(tag, ",") {
		if strings.TrimSpace(part) == token {
			return true
		}
	}
	return false
}

// lookup walks a source's nested map by path, falling back to a flat dotted
// key for sources that return flat maps.
func lookup(m map[string]any, path []string) (any, bool) {
	cur := any(m)
	for i, seg := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			break
		}
		v, ok := mm[seg]
		if !ok {
			break
		}
		if i == len(path)-1 {
			return v, true
		}
		cur = v
	}
	v, ok := m[strings.Join(path, ".")]
	return v, ok
}

func errRequired(f field, s settings) error {
	var fixes []string
	if s.useEnv {
		fixes = append(fixes, "set "+f.envName(s.envPrefix))
	}
	if len(f.path) > 1 {
		fixes = append(fixes, fmt.Sprintf("add %q under %q in a file source", f.path[len(f.path)-1], strings.Join(f.path[:len(f.path)-1], ".")))
	} else {
		fixes = append(fixes, fmt.Sprintf("add %q in a file source", f.path[0]))
	}
	if s.flags != nil {
		fixes = append(fixes, "pass -"+f.key())
	}
	last := len(fixes) - 1
	list := strings.Join(fixes[:last], ", ")
	if list != "" {
		list += ", or " + fixes[last]
	} else {
		list = fixes[last]
	}
	return fmt.Errorf("config: %s is required and no layer set it — %s", f.key(), list)
}

// convert coerces a layer's value into the field's type. Sources hand over
// what their parser produced; env and flags hand over strings.
func convert(value any, t reflect.Type) (reflect.Value, error) {
	rv := reflect.ValueOf(value)
	if rv.IsValid() && rv.Type() == t {
		return rv, nil
	}

	switch t.Kind() {
	case reflect.String:
		if s, ok := value.(string); ok {
			return reflect.ValueOf(s).Convert(t), nil
		}
	case reflect.Bool:
		switch v := value.(type) {
		case bool:
			return reflect.ValueOf(v).Convert(t), nil
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(b).Convert(t), nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if t == durationType {
			if s, ok := value.(string); ok {
				d, err := time.ParseDuration(s)
				if err != nil {
					return reflect.Value{}, err
				}
				return reflect.ValueOf(d), nil
			}
			// A bare number would silently mean nanoseconds — a 30-second
			// intent becoming a 30ns timeout. Refuse.
			return reflect.Value{}, hint(`a duration needs a unit — write it as a string like "30s"`)
		}
		switch v := value.(type) {
		case string:
			n, err := strconv.ParseInt(v, 10, t.Bits())
			if err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(n).Convert(t), nil
		default:
			if rv.IsValid() && rv.CanInt() {
				return intInRange(rv.Int(), t)
			}
			if rv.IsValid() && rv.CanFloat() && rv.Float() == float64(int64(rv.Float())) {
				return intInRange(int64(rv.Float()), t)
			}
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		switch v := value.(type) {
		case string:
			n, err := strconv.ParseUint(v, 10, t.Bits())
			if err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(n).Convert(t), nil
		default:
			if rv.IsValid() && rv.CanInt() && rv.Int() >= 0 {
				n := uint64(rv.Int())
				if reflect.New(t).Elem().OverflowUint(n) {
					return reflect.Value{}, hint(fmt.Sprintf("%d overflows %s", n, t))
				}
				return reflect.ValueOf(n).Convert(t), nil
			}
		}
	case reflect.Float32, reflect.Float64:
		switch v := value.(type) {
		case string:
			f, err := strconv.ParseFloat(v, t.Bits())
			if err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(f).Convert(t), nil
		default:
			if rv.IsValid() && (rv.CanFloat() || rv.CanInt()) {
				return rv.Convert(t), nil
			}
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			switch v := value.(type) {
			case string:
				parts := strings.Split(v, ",")
				out := reflect.MakeSlice(t, len(parts), len(parts))
				for i, p := range parts {
					out.Index(i).SetString(strings.TrimSpace(p))
				}
				return out, nil
			case []any:
				out := reflect.MakeSlice(t, len(v), len(v))
				for i, e := range v {
					s, ok := e.(string)
					if !ok {
						return reflect.Value{}, fmt.Errorf("element %d is %T, not a string", i, e)
					}
					out.Index(i).SetString(s)
				}
				return out, nil
			case []string:
				return rv.Convert(t), nil
			}
		}
	case reflect.Pointer, reflect.Map:
		return reflect.Value{}, hint("pointer and map fields are not supported — use a value struct or a supported leaf type")
	}
	return reflect.Value{}, fmt.Errorf("unsupported conversion from %T", value)
}

// intInRange converts n to the signed integer type t, refusing a value the
// type cannot hold — silent wrap-around is a garbage config that boots.
func intInRange(n int64, t reflect.Type) (reflect.Value, error) {
	if reflect.New(t).Elem().OverflowInt(n) {
		return reflect.Value{}, hint(fmt.Sprintf("%d overflows %s", n, t))
	}
	return reflect.ValueOf(n).Convert(t), nil
}

// quote renders a layer's value for an error message: strings quoted, other
// parser-produced values as they print.
func quote(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%v", v)
}
