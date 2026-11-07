package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"codedeep/internal/diff"
)

const maxPatchEdits = 100

type patchEdit struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

type patchArgs struct {
	Edits []patchEdit `json:"edits"`
}

// patchTarget tracks one file across every edit that mentions it, so repeated
// hunks apply in order to the same in-memory content and the checkpoint keeps
// a single snapshot of the original file.
type patchTarget struct {
	rel          string
	abs          string
	mode         os.FileMode
	old          string
	current      string
	edits        int
	replacements int
}

type patchPlan struct {
	targets []*patchTarget
}

func (f *FS) patchKey(abs string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(abs)
	}
	return abs
}

func (f *FS) loadPatchTarget(abs string) (*patchTarget, error) {
	rel := f.Rel(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", rel)
	}
	if info.Size() > 20*1024*1024 {
		return nil, fmt.Errorf("%s is too large to patch", rel)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if isBinary(data) {
		return nil, fmt.Errorf("%s appears to be a binary file", rel)
	}
	text := string(data)
	return &patchTarget{rel: rel, abs: abs, mode: info.Mode().Perm(), old: text, current: text}, nil
}

// planPatch validates every edit without touching the workspace. Edits that
// target the same file are applied to the accumulated content in order.
func (f *FS) planPatch(raw json.RawMessage) (*patchPlan, error) {
	var a patchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if len(a.Edits) == 0 {
		return nil, errors.New("apply_patch: edits is required and must not be empty")
	}
	if len(a.Edits) > maxPatchEdits {
		return nil, fmt.Errorf("apply_patch: %d edits exceeds the limit of %d", len(a.Edits), maxPatchEdits)
	}
	byKey := make(map[string]*patchTarget, len(a.Edits))
	plan := &patchPlan{}
	for i, e := range a.Edits {
		if strings.TrimSpace(e.Path) == "" {
			return nil, fmt.Errorf("apply_patch: edit %d: path is required", i+1)
		}
		if e.OldString == "" {
			return nil, fmt.Errorf("apply_patch: edit %d (%s): old_string is required and must not be empty", i+1, e.Path)
		}
		if e.OldString == e.NewString {
			return nil, fmt.Errorf("apply_patch: edit %d (%s): old_string and new_string are identical", i+1, e.Path)
		}
		abs, err := f.Resolve(e.Path)
		if err != nil {
			return nil, fmt.Errorf("apply_patch: edit %d: %w", i+1, err)
		}
		key := f.patchKey(abs)
		target := byKey[key]
		if target == nil {
			target, err = f.loadPatchTarget(abs)
			if err != nil {
				return nil, fmt.Errorf("apply_patch: edit %d (%s): %w", i+1, f.Rel(abs), err)
			}
			byKey[key] = target
			plan.targets = append(plan.targets, target)
		}
		updated, count, _, err := replaceText(target.current, e.OldString, e.NewString, e.ReplaceAll, target.rel)
		if err != nil {
			return nil, fmt.Errorf("apply_patch: edit %d: %w", i+1, err)
		}
		target.current = updated
		target.edits++
		target.replacements += count
	}
	return plan, nil
}

func (f *FS) previewPatch(ctx context.Context, raw json.RawMessage) (*Preview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan, err := f.planPatch(raw)
	if err != nil {
		return nil, err
	}
	var files []string
	var parts []string
	for _, t := range plan.targets {
		if t.old == t.current {
			continue
		}
		files = append(files, t.rel)
		if len(t.old) > maxPreviewBytes || len(t.current) > maxPreviewBytes {
			parts = append(parts, fmt.Sprintf("[%s is too large to preview; the patch will still apply]", t.rel))
			continue
		}
		if d := diff.Unified(t.rel, t.old, t.current, 3); d != "" {
			parts = append(parts, d)
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	return &Preview{Path: patchLabel(files), Diff: strings.Join(parts, "\n")}, nil
}

func patchLabel(files []string) string {
	switch len(files) {
	case 0:
		return ""
	case 1:
		return files[0]
	case 2:
		return files[0] + ", " + files[1]
	default:
		return fmt.Sprintf("%s (+%d more)", files[0], len(files)-1)
	}
}

func (f *FS) applyPatch(ctx context.Context, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	plan, err := f.planPatch(raw)
	if err != nil {
		return Result{}, err
	}

	var written, changed []*patchTarget
	for _, t := range plan.targets {
		if t.old == t.current {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Result{}, errors.Join(append([]error{err}, rollbackPatch(written)...)...)
		}
		if err := os.WriteFile(t.abs, []byte(t.current), t.mode); err != nil {
			errs := []error{fmt.Errorf("apply_patch: write %s: %w", t.rel, err)}
			return Result{}, errors.Join(append(errs, rollbackPatch(written)...)...)
		}
		written = append(written, t)
		changed = append(changed, t)
	}
	if len(changed) == 0 {
		return Result{Output: "apply_patch: no files changed"}, nil
	}

	edits, replacements := 0, 0
	for _, t := range changed {
		edits += t.edits
		replacements += t.replacements
	}
	var b strings.Builder
	fmt.Fprintf(&b, "applied %d edit(s) to %d file(s) (%d replacement(s))", edits, len(changed), replacements)
	var changes []*Change
	for _, t := range changed {
		fmt.Fprintf(&b, "\n- %s: %d edit(s), %d replacement(s)", t.rel, t.edits, t.replacements)
		if len(t.old) <= maxSnapshotBytes && len(t.current) <= maxSnapshotBytes {
			changes = append(changes, &Change{
				Path:    t.rel,
				Existed: true,
				Mode:    uint32(t.mode),
				Old:     t.old,
				New:     t.current,
			})
		}
	}
	return Result{Output: b.String(), Changes: changes}, nil
}

// rollbackPatch restores files already written before a later write failed, so
// a failed apply_patch leaves every target unchanged.
func rollbackPatch(written []*patchTarget) []error {
	var errs []error
	for i := len(written) - 1; i >= 0; i-- {
		t := written[i]
		if err := os.WriteFile(t.abs, []byte(t.old), t.mode); err != nil {
			errs = append(errs, fmt.Errorf("rollback %s: %w", t.rel, err))
		}
	}
	return errs
}
