package patch

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
	"gopkg.in/yaml.v3"
)

type File struct {
	Patches []Operation `yaml:"patches"`
}

type Operation struct {
	CIDR  string         `yaml:"cidr" json:"cidr"`
	Op    string         `yaml:"op" json:"op"`
	Set   map[string]any `yaml:"set" json:"set,omitempty"`
	Field string         `yaml:"field" json:"field,omitempty"`
}

type Report struct {
	Total   int      `json:"total"`
	Changed int      `json:"changed"`
	Changes []Change `json:"changes"`
}

type Change struct {
	CIDR   string `json:"cidr"`
	Op     string `json:"op"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

func LoadFile(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read patch file: %w", err)
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("parse patch file: %w", err)
	}
	return f, nil
}

func (f File) Validate() error {
	if len(f.Patches) == 0 {
		return errors.New("patch file must contain at least one patch")
	}
	for i, p := range f.Patches {
		if p.CIDR == "" {
			return fmt.Errorf("patch %d: cidr is required", i)
		}
		if _, _, err := net.ParseCIDR(p.CIDR); err != nil {
			return fmt.Errorf("patch %d: invalid cidr %q: %w", i, p.CIDR, err)
		}
		switch p.Op {
		case "merge", "replace":
			if len(p.Set) == 0 {
				return fmt.Errorf("patch %d: set is required for op %q", i, p.Op)
			}
		case "delete_field":
			if p.Field == "" {
				return fmt.Errorf("patch %d: field is required for delete_field", i)
			}
		case "delete_record", "delete":
		default:
			return fmt.Errorf("patch %d: unsupported op %q", i, p.Op)
		}
	}
	return nil
}

func Apply(tree *mmdbwriter.Tree, f File) error {
	for _, p := range f.Patches {
		_, network, err := net.ParseCIDR(p.CIDR)
		if err != nil {
			return fmt.Errorf("parse cidr %q: %w", p.CIDR, err)
		}
		fn, err := Inserter(p)
		if err != nil {
			return err
		}
		if err := tree.InsertFunc(network, fn); err != nil {
			return fmt.Errorf("apply %s to %s: %w", p.Op, p.CIDR, err)
		}
	}
	return nil
}

func Diff(reader *maxminddb.Reader, f File) (Report, error) {
	report := Report{Total: len(f.Patches)}
	for _, p := range f.Patches {
		ip, _, err := net.ParseCIDR(p.CIDR)
		if err != nil {
			return Report{}, fmt.Errorf("parse cidr %q: %w", p.CIDR, err)
		}
		before, err := lookup(reader, ip)
		if err != nil {
			return Report{}, fmt.Errorf("lookup %s: %w", p.CIDR, err)
		}
		beforeType, err := ToMMDBType(before)
		if err != nil {
			return Report{}, fmt.Errorf("convert existing value for %s: %w", p.CIDR, err)
		}
		fn, err := Inserter(p)
		if err != nil {
			return Report{}, err
		}
		afterType, err := fn(beforeType)
		if err != nil {
			return Report{}, fmt.Errorf("diff %s: %w", p.CIDR, err)
		}
		after := FromMMDBType(afterType)
		if !reflect.DeepEqual(before, after) {
			report.Changed++
		}
		report.Changes = append(report.Changes, Change{
			CIDR:   p.CIDR,
			Op:     p.Op,
			Before: before,
			After:  after,
		})
	}
	return report, nil
}

func Inserter(p Operation) (inserter.Func, error) {
	switch p.Op {
	case "merge":
		value, err := ToMMDBType(ExpandDottedMap(p.Set))
		if err != nil {
			return nil, fmt.Errorf("convert merge value for %s: %w", p.CIDR, err)
		}
		return inserter.DeepMergeWith(value), nil
	case "replace":
		value, err := ToMMDBType(ExpandDottedMap(p.Set))
		if err != nil {
			return nil, fmt.Errorf("convert replace value for %s: %w", p.CIDR, err)
		}
		return inserter.ReplaceWith(value), nil
	case "delete_field":
		return func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
			return DeletePath(existing, strings.Split(p.Field, "."))
		}, nil
	case "delete_record", "delete":
		return inserter.Remove, nil
	default:
		return nil, fmt.Errorf("unsupported op %q", p.Op)
	}
}

func lookup(reader *maxminddb.Reader, ip net.IP) (any, error) {
	var record map[string]any
	if err := reader.Lookup(ip, &record); err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	return record, nil
}

func ExpandDottedMap(in map[string]any) map[string]any {
	out := map[string]any{}
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		setPath(out, strings.Split(key, "."), in[key])
	}
	return out
}

func setPath(dst map[string]any, path []string, value any) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		if nested, ok := value.(map[string]any); ok {
			dst[path[0]] = ExpandDottedMap(nested)
			return
		}
		dst[path[0]] = value
		return
	}
	next, _ := dst[path[0]].(map[string]any)
	if next == nil {
		next = map[string]any{}
		dst[path[0]] = next
	}
	setPath(next, path[1:], value)
}

func DeletePath(existing mmdbtype.DataType, path []string) (mmdbtype.DataType, error) {
	if len(path) == 0 || existing == nil {
		return existing, nil
	}
	m, ok := cloneMap(existing)
	if !ok {
		return existing, nil
	}
	deletePath(m, path)
	return m, nil
}

func deletePath(m mmdbtype.Map, path []string) {
	if len(path) == 1 {
		delete(m, mmdbtype.String(path[0]))
		return
	}
	child, ok := m[mmdbtype.String(path[0])].(mmdbtype.Map)
	if !ok {
		return
	}
	deletePath(child, path[1:])
}

func cloneMap(v mmdbtype.DataType) (mmdbtype.Map, bool) {
	m, ok := v.(mmdbtype.Map)
	if !ok {
		return nil, false
	}
	out := make(mmdbtype.Map, len(m))
	for k, value := range m {
		if child, ok := cloneMap(value); ok {
			out[k] = child
			continue
		}
		out[k] = value
	}
	return out, true
}

func ToMMDBType(v any) (mmdbtype.DataType, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case mmdbtype.DataType:
		return x, nil
	case map[string]any:
		m := mmdbtype.Map{}
		for key, value := range x {
			converted, err := ToMMDBType(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			if converted != nil {
				m[mmdbtype.String(key)] = converted
			}
		}
		return m, nil
	case map[any]any:
		m := mmdbtype.Map{}
		for key, value := range x {
			keyString, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("map key %v is not a string", key)
			}
			converted, err := ToMMDBType(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", keyString, err)
			}
			if converted != nil {
				m[mmdbtype.String(keyString)] = converted
			}
		}
		return m, nil
	case []any:
		s := make(mmdbtype.Slice, 0, len(x))
		for _, item := range x {
			converted, err := ToMMDBType(item)
			if err != nil {
				return nil, err
			}
			if converted != nil {
				s = append(s, converted)
			}
		}
		return s, nil
	case string:
		return mmdbtype.String(x), nil
	case bool:
		return mmdbtype.Bool(x), nil
	case int32:
		return mmdbtype.Int32(x), nil
	case int:
		return mmdbtype.Int32(x), nil
	case int64:
		return mmdbtype.Int32(x), nil
	case uint16:
		return mmdbtype.Uint16(x), nil
	case uint32:
		return mmdbtype.Uint32(x), nil
	case uint:
		return mmdbtype.Uint32(x), nil
	case uint64:
		return mmdbtype.Uint64(x), nil
	case float32:
		return mmdbtype.Float32(x), nil
	case float64:
		return mmdbtype.Float64(x), nil
	default:
		return nil, fmt.Errorf("unsupported value type %T", v)
	}
}

func FromMMDBType(v mmdbtype.DataType) any {
	switch x := v.(type) {
	case nil:
		return nil
	case mmdbtype.Map:
		m := map[string]any{}
		for key, value := range x {
			m[string(key)] = FromMMDBType(value)
		}
		return m
	case mmdbtype.Slice:
		s := make([]any, 0, len(x))
		for _, item := range x {
			s = append(s, FromMMDBType(item))
		}
		return s
	case mmdbtype.String:
		return string(x)
	case mmdbtype.Bool:
		return bool(x)
	case mmdbtype.Int32:
		return int32(x)
	case mmdbtype.Uint16:
		return uint16(x)
	case mmdbtype.Uint32:
		return uint32(x)
	case mmdbtype.Uint64:
		return uint64(x)
	case mmdbtype.Float32:
		return float32(x)
	case mmdbtype.Float64:
		return float64(x)
	case mmdbtype.Bytes:
		return []byte(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
