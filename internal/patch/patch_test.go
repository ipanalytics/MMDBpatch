package patch

import (
	"reflect"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

func TestExpandDottedMap(t *testing.T) {
	got := ExpandDottedMap(map[string]any{
		"custom.source":        "manual_override",
		"geo.country.iso_code": "DE",
	})
	want := map[string]any{
		"custom": map[string]any{
			"source": "manual_override",
		},
		"geo": map[string]any{
			"country": map[string]any{
				"iso_code": "DE",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandDottedMap() = %#v, want %#v", got, want)
	}
}

func TestDeletePath(t *testing.T) {
	existing := mmdbtype.Map{
		"traits": mmdbtype.Map{
			"is_anonymous_proxy": mmdbtype.Bool(true),
			"user_type":          mmdbtype.String("business"),
		},
	}
	got, err := DeletePath(existing, []string{"traits", "is_anonymous_proxy"})
	if err != nil {
		t.Fatal(err)
	}
	want := mmdbtype.Map{
		"traits": mmdbtype.Map{
			"user_type": mmdbtype.String("business"),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeletePath() = %#v, want %#v", got, want)
	}
}

func TestValidateRequiresSetForMerge(t *testing.T) {
	f := File{Patches: []Operation{{CIDR: "203.0.113.0/24", Op: "merge"}}}
	if err := f.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
}
