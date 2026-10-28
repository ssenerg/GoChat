package config

import (
	"testing"
)

type item struct {
	ID    string   `mapstructure:"id"`
	Names []string `mapstructure:"names"`
}

type listed struct {
	Items []item   `mapstructure:"items"`
	Tags  []string `mapstructure:"tags"`
}

func (*listed) Validate() error { return nil }

func TestListsComeAsJSONFromTheEnvironment(t *testing.T) {
	defaults := []byte("items: []\ntags: []\n")
	t.Setenv("TEST_ITEMS", `[{"id": "a", "names": ["x", "y"]}, {"id": "b"}]`)
	t.Setenv("TEST_TAGS", "p,q")
	var c listed
	if err := LoadConfig(&c, "TEST", t.TempDir(), "none", defaults); err != nil {
		t.Fatal(err)
	}
	if len(c.Items) != 2 || c.Items[0].ID != "a" || len(c.Items[0].Names) != 2 || c.Items[1].ID != "b" {
		t.Fatalf("items: %+v", c.Items)
	}
	if len(c.Tags) != 2 || c.Tags[1] != "q" {
		t.Fatalf("tags: %v", c.Tags)
	}

	t.Setenv("TEST_ITEMS", `[{"id": "a", "nmes": ["x"]}]`)
	if err := LoadConfig(&listed{}, "TEST", t.TempDir(), "none", defaults); err == nil {
		t.Fatal("an unknown key in the JSON was taken")
	}
	t.Setenv("TEST_ITEMS", `{"id": "a"`)
	if err := LoadConfig(&listed{}, "TEST", t.TempDir(), "none", defaults); err == nil {
		t.Fatal("broken JSON was taken")
	}
}
