package packagecontent

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestAutoloadDisabledPatternsKeepTouchedPathsAndLastState(t *testing.T) {
	// package-manager.ts:787-804 preserves first-match Map order while later patterns overwrite values. Plus/minus are exact, not glob patterns.
	root := t.TempDir()
	foo, bar := filepath.Join(root, "foo.ts"), filepath.Join(root, "bar.ts")
	for _, tc := range []struct {
		name     string
		patterns []string
		want     []PatternState
	}{
		{"absent", nil, []PatternState{}}, {"empty", []string{}, []PatternState{}},
		{"negative globs touch", []string{"!*.ts"}, []PatternState{{foo, false}, {bar, false}}},
		{"exact positive is not glob", []string{"+*.ts"}, []PatternState{}},
		{"outside exact cannot add file", []string{"+../outside"}, []PatternState{}},
		{"force overrides last", []string{"!*.ts", "+bar.ts"}, []PatternState{{foo, false}, {bar, true}}},
		{"first insertion last value", []string{"+bar.ts", "foo.ts", "-bar.ts"}, []PatternState{{bar, false}, {foo, true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyAutoloadDisabledPatterns([]string{foo, bar}, tc.patterns, root, Extensions)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("states=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestAutoloadDisabledSkillParentMatch(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "skills", "foo", "SKILL.md")
	got := ApplyAutoloadDisabledPatterns([]string{skill}, []string{"+skills/foo"}, root, Skills)
	if !reflect.DeepEqual(got, []PatternState{{skill, true}}) {
		t.Fatalf("states=%v", got)
	}
}
