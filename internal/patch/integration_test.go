package patch

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
)

func TestApplyWritesPatchedMMDB(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mmdb")
	output := filepath.Join(dir, "output.mmdb")

	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "mmdbpatch-test",
		Description:             map[string]string{"en": "mmdbpatch test"},
		IPVersion:               4,
		IncludeReservedNetworks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, network, err := net.ParseCIDR("203.0.113.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Insert(network, mmdbtype.Map{
		"geo": mmdbtype.Map{
			"country": mmdbtype.Map{
				"iso_code": mmdbtype.String("US"),
			},
		},
		"traits": mmdbtype.Map{
			"is_anonymous_proxy": mmdbtype.Bool(true),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeTree(input, tree); err != nil {
		t.Fatal(err)
	}

	loaded, err := mmdbwriter.Load(input, mmdbwriter.Options{IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(loaded, File{Patches: []Operation{
		{
			CIDR: "203.0.113.0/24",
			Op:   "merge",
			Set: map[string]any{
				"custom.source":        "manual_override",
				"geo.country.iso_code": "DE",
			},
		},
		{
			CIDR:  "203.0.113.0/24",
			Op:    "delete_field",
			Field: "traits.is_anonymous_proxy",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeTree(output, loaded); err != nil {
		t.Fatal(err)
	}

	reader, err := maxminddb.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	var got map[string]any
	if err := reader.Lookup(net.ParseIP("203.0.113.1"), &got); err != nil {
		t.Fatal(err)
	}
	country := got["geo"].(map[string]any)["country"].(map[string]any)
	if country["iso_code"] != "DE" {
		t.Fatalf("iso_code = %v, want DE", country["iso_code"])
	}
	custom := got["custom"].(map[string]any)
	if custom["source"] != "manual_override" {
		t.Fatalf("custom.source = %v, want manual_override", custom["source"])
	}
	traits := got["traits"].(map[string]any)
	if _, ok := traits["is_anonymous_proxy"]; ok {
		t.Fatal("traits.is_anonymous_proxy was not deleted")
	}
}

func writeTree(path string, tree *mmdbwriter.Tree) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = tree.WriteTo(f)
	return err
}
