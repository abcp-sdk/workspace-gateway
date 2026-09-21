package presets

import (
	"encoding/json"
	"testing"

	"github.com/abcp-sdk/workspace-gateway/internal/roles"
)

func TestAllPresetsValid(t *testing.T) {
	all := All()
	ids := map[string]bool{}
	for _, p := range all {
		if p.ID == "" || p.SystemPrompt == "" {
			t.Fatalf("preset %+v incomplete", p)
		}
		if len(p.Tools) == 0 {
			t.Fatalf("preset %s has an empty whitelist (== all tools)", p.ID)
		}
		ids[p.ID] = true
	}
	for _, want := range []string{"admin", "maintainer", "developer", "planner", "explorer"} {
		if !ids[want] {
			t.Fatalf("missing preset %s", want)
		}
	}
}

func TestJSONRoundTrips(t *testing.T) {
	b, err := JSON()
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(b, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != len(All()) {
		t.Fatalf("round-trip lost entries")
	}
}

func TestMaintainerPresetMatchesRole(t *testing.T) {
	for _, p := range All() {
		if p.ID == "maintainer" {
			if len(p.Tools) != len(roles.ToolsFor(roles.Maintainer)) {
				t.Fatal("maintainer preset tools drift from roles.ToolsFor")
			}
		}
	}
}
