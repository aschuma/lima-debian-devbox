package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestExampleConfigurations(t *testing.T) {
	// Isolate HOME so no test depends on the checkout's location under the real
	// user's home. Relative mount paths remain rooted in each example directory.
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"native", "ssh", "none"} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig("examples/" + name + ".yaml"); err != nil {
				t.Fatal(err)
			}
		})
	}
	data, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatal("missing schema dialect")
	}
}
