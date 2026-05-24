package patch

import (
	"encoding/json"
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

const (
	ConflictPatchWins   = "patch_wins"
	ConflictFirstWins   = "first_wins"
	ConflictFailOverlap = "fail_on_overlap"
)

type File struct {
	Defaults Defaults    `yaml:"defaults" json:"defaults,omitempty"`
	Patches  []Operation `yaml:"patches" json:"patches"`
}

type Defaults struct {
	Conflict string `yaml:"conflict" json:"conflict,omitempty"`
}

type Operation struct {
	CIDR     string         `yaml:"cidr" json:"cidr"`
	Op       string         `yaml:"op" json:"op"`
	Conflict string         `yaml:"conflict" json:"conflict,omitempty"`
	Set      map[string]any `yaml:"set" json:"set,omitempty"`
	Field    string         `yaml:"field" json:"field,omitempty"`
}

type Report struct {
	Total             int            `json:"total"`
	Applied           int            `json:"applied"`
	Skipped           int            `json:"skipped"`
	Changed           int            `json:"changed"`
	AffectedNetworks  int            `json:"affected_networks"`
	ChangedNetworks   int            `json:"changed_networks"`
	UnchangedNetworks int            `json:"unchanged_networks"`
	FieldsChanged     []string       `json:"fields_changed,omitempty"`
	Changes           []Change       `json:"changes"`
	SkippedPatches    []SkippedPatch `json:"skipped_patches,omitempty"`
}

type Change struct {
	CIDR          string   `json:"cidr"`
	Network       string   `json:"network"`
	Op            string   `json:"op"`
	Changed       bool     `json:"changed"`
	FieldsChanged []string `json:"fields_changed,omitempty"`
	Before        any      `json:"before"`
	After         any      `json:"after"`
}

type SkippedPatch struct {
	CIDR   string `json:"cidr"`
	Op     string `json:"op"`
	Reason string `json:"reason"`
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
	if err := validateConflict("defaults.conflict", f.Defaults.Conflict); err != nil {
		return err
	}
	for i, p := range f.Patches {
		if p.CIDR == "" {
			return fmt.Errorf("patch %d: cidr is required", i)
		}
		if _, _, err := net.ParseCIDR(p.CIDR); err != nil {
			return fmt.Errorf("patch %d: invalid cidr %q: %w", i, p.CIDR, err)
		}
		if err := validateConflict(fmt.Sprintf("patch %d conflict", i), p.Conflict); err != nil {
			return err
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
	_, _, err := effectiveOperations(f)
	return err
}

func Apply(tree *mmdbwriter.Tree, f File) error {
	operations, _, err := effectiveOperations(f)
	if err != nil {
		return err
	}
	for _, p := range operations {
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
	operations, skipped, err := effectiveOperations(f)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		Total:          len(f.Patches),
		Applied:        len(operations),
		Skipped:        len(skipped),
		SkippedPatches: skipped,
	}
	fieldsChanged := map[string]struct{}{}
	for _, p := range operations {
		_, patchNetwork, err := net.ParseCIDR(p.CIDR)
		if err != nil {
			return Report{}, fmt.Errorf("parse cidr %q: %w", p.CIDR, err)
		}
		changes, err := diffOperation(reader, p, patchNetwork)
		if err != nil {
			return Report{}, err
		}
		for _, change := range changes {
			report.AffectedNetworks++
			if change.Changed {
				report.Changed++
				report.ChangedNetworks++
			} else {
				report.UnchangedNetworks++
			}
			for _, field := range change.FieldsChanged {
				fieldsChanged[field] = struct{}{}
			}
			report.Changes = append(report.Changes, change)
		}
	}
	report.FieldsChanged = sortedKeys(fieldsChanged)
	return report, nil
}

func WriteReport(path string, report Report) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

func diffOperation(reader *maxminddb.Reader, p Operation, patchNetwork *net.IPNet) ([]Change, error) {
	networks := reader.NetworksWithin(patchNetwork, maxminddb.SkipAliasedNetworks)
	var changes []Change
	for networks.Next() {
		var before map[string]any
		network, err := networks.Network(&before)
		if err != nil {
			return nil, fmt.Errorf("read network in %s: %w", p.CIDR, err)
		}
		change, err := diffRecord(p, network.String(), before)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	if err := networks.Err(); err != nil {
		return nil, fmt.Errorf("iterate networks within %s: %w", p.CIDR, err)
	}
	if len(changes) == 0 {
		change, err := diffRecord(p, patchNetwork.String(), nil)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func diffRecord(p Operation, network string, before any) (Change, error) {
	beforeType, err := ToMMDBType(before)
	if err != nil {
		return Change{}, fmt.Errorf("convert existing value for %s: %w", p.CIDR, err)
	}
	fn, err := Inserter(p)
	if err != nil {
		return Change{}, err
	}
	afterType, err := fn(beforeType)
	if err != nil {
		return Change{}, fmt.Errorf("diff %s: %w", p.CIDR, err)
	}
	after := FromMMDBType(afterType)
	fields := ChangedFields(before, after)
	return Change{
		CIDR:          p.CIDR,
		Network:       network,
		Op:            p.Op,
		Changed:       !reflect.DeepEqual(before, after),
		FieldsChanged: fields,
		Before:        before,
		After:         after,
	}, nil
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

func effectiveOperations(f File) ([]Operation, []SkippedPatch, error) {
	var operations []Operation
	var networks []*net.IPNet
	var skipped []SkippedPatch
	for _, p := range f.Patches {
		_, network, err := net.ParseCIDR(p.CIDR)
		if err != nil {
			return nil, nil, fmt.Errorf("parse cidr %q: %w", p.CIDR, err)
		}
		conflict := operationConflict(f, p)
		for i, existing := range networks {
			if overlaps(existing, network) {
				switch conflict {
				case ConflictFailOverlap:
					return nil, nil, fmt.Errorf("patch %s overlaps earlier patch %s", p.CIDR, operations[i].CIDR)
				case ConflictFirstWins:
					skipped = append(skipped, SkippedPatch{
						CIDR:   p.CIDR,
						Op:     p.Op,
						Reason: fmt.Sprintf("overlaps earlier patch %s", operations[i].CIDR),
					})
					goto nextPatch
				}
			}
		}
		operations = append(operations, p)
		networks = append(networks, network)
	nextPatch:
	}
	return operations, skipped, nil
}

func operationConflict(f File, p Operation) string {
	if p.Conflict != "" {
		return p.Conflict
	}
	return defaultConflict(f)
}

func defaultConflict(f File) string {
	if f.Defaults.Conflict != "" {
		return f.Defaults.Conflict
	}
	return ConflictPatchWins
}

func validateConflict(name, conflict string) error {
	switch conflict {
	case "", ConflictPatchWins, ConflictFirstWins, ConflictFailOverlap:
		return nil
	default:
		return fmt.Errorf("%s: unsupported conflict strategy %q", name, conflict)
	}
}

func overlaps(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func ChangedFields(before, after any) []string {
	beforeFlat := map[string]any{}
	afterFlat := map[string]any{}
	flatten("", before, beforeFlat)
	flatten("", after, afterFlat)

	seen := map[string]struct{}{}
	for key, beforeValue := range beforeFlat {
		if !reflect.DeepEqual(beforeValue, afterFlat[key]) {
			seen[key] = struct{}{}
		}
	}
	for key, afterValue := range afterFlat {
		if !reflect.DeepEqual(beforeFlat[key], afterValue) {
			seen[key] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

func flatten(prefix string, value any, out map[string]any) {
	switch x := value.(type) {
	case nil:
		return
	case map[string]any:
		for key, child := range x {
			childPrefix := key
			if prefix != "" {
				childPrefix = prefix + "." + key
			}
			flatten(childPrefix, child, out)
		}
	default:
		if prefix != "" {
			out[prefix] = x
		}
	}
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
