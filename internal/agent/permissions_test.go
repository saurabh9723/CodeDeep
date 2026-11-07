package agent

import (
	"encoding/json"
	"testing"

	"codedeep/internal/approval"
	"codedeep/internal/config"
)

func patchArgs(paths ...string) json.RawMessage {
	type edit struct {
		Path string `json:"path"`
	}
	edits := make([]edit, 0, len(paths))
	for _, p := range paths {
		edits = append(edits, edit{Path: p})
	}
	raw, err := json.Marshal(map[string]any{"edits": edits})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestApplyPatchDenyMatchesAnyEditPath(t *testing.T) {
	cfg := &config.Config{
		WorkDir: t.TempDir(),
		Permissions: []config.PermissionRule{
			{Tool: "apply_patch", Action: "deny", Path: "**/*.env"},
		},
	}
	a := &Agent{cfg: cfg}

	dec, set := a.evaluateRules("apply_patch", patchArgs("src/a.go", "src/secret.env"))
	if !set || dec != approval.Deny {
		t.Fatalf("deny rule should match when any edit touches a protected file: %v, %v", dec, set)
	}
	if _, set := a.evaluateRules("apply_patch", patchArgs("src/a.go", "src/b.go")); set {
		t.Fatal("deny rule should not match unrelated paths")
	}
}

func TestApplyPatchAllowRequiresEveryEditPath(t *testing.T) {
	cfg := &config.Config{
		WorkDir: t.TempDir(),
		Permissions: []config.PermissionRule{
			{Tool: "apply_patch", Action: "allow", Path: "src/**"},
		},
	}
	a := &Agent{cfg: cfg}

	dec, set := a.evaluateRules("apply_patch", patchArgs("src/a.go", "src/b.go"))
	if !set || dec != approval.AllowOnce {
		t.Fatalf("allow rule should cover a patch entirely inside src: %v, %v", dec, set)
	}
	if _, set := a.evaluateRules("apply_patch", patchArgs("src/a.go", "docs/b.md")); set {
		t.Fatal("allow rule must not cover a patch that edits outside src")
	}
}

func TestApplyPatchDenyWinsOverPathAllow(t *testing.T) {
	cfg := &config.Config{
		WorkDir: t.TempDir(),
		Permissions: []config.PermissionRule{
			{Tool: "apply_patch", Action: "allow", Path: "src/**"},
			{Tool: "apply_patch", Action: "deny", Path: "**/*.env"},
		},
	}
	a := &Agent{cfg: cfg}

	dec, set := a.evaluateRules("apply_patch", patchArgs("src/a.go", "src/secret.env"))
	if !set || dec != approval.Deny {
		t.Fatalf("deny must override a partial allow: %v, %v", dec, set)
	}
}

func TestAllowKeyNarrowsApplyPatchToPathSet(t *testing.T) {
	first := allowKey("apply_patch", `{"edits":[{"path":"b.go"},{"path":"a.go"}]}`)
	reordered := allowKey("apply_patch", `{"edits":[{"path":"a.go"},{"path":"b.go"}]}`)
	other := allowKey("apply_patch", `{"edits":[{"path":"a.go"},{"path":"c.go"}]}`)
	if first != reordered {
		t.Errorf("path order changed the allow key: %q vs %q", first, reordered)
	}
	if first == other {
		t.Error("different path sets share an allow key")
	}
	if first == "apply_patch" {
		t.Error("apply_patch allow key was not narrowed to its paths")
	}
}
