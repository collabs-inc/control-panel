package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListSkillsFromEverywhereClaudeLooks(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	put := func(p, body string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	skill := func(name, desc string) string {
		return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# x\n"
	}
	put(filepath.Join(home, ".claude/skills/cube-browser/SKILL.md"), skill("cube-browser", "Use the browser."))
	put(filepath.Join(home, ".claude/skills/synced/old/docx/SKILL.md"), skill("docx", "Old account."))
	put(filepath.Join(home, ".claude/skills/synced/new/pdf/SKILL.md"), skill("pdf", "Read PDFs."))
	os.Chtimes(filepath.Join(home, ".claude/skills/synced/old"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	put(filepath.Join(home, ".claude/commands/standup.md"), "---\ndescription: Write my standup\n---\nbody")
	pon := filepath.Join(home, ".claude/plugins/cache/ponytail/ponytail/4.10.0")
	put(filepath.Join(pon, "skills/ponytail/SKILL.md"), "---\nname: ponytail\ndescription: >\n  Forces the laziest solution\n  that works.\n---\n")
	off := filepath.Join(home, ".claude/plugins/cache/x/off/1.0")
	put(filepath.Join(off, "skills/nope/SKILL.md"), skill("nope", "Disabled plugin."))
	put(filepath.Join(home, ".claude/settings.json"), `{"enabledPlugins":{"ponytail@ponytail":true,"off@x":false}}`)
	put(filepath.Join(home, ".claude/plugins/installed_plugins.json"), `{"version":2,"plugins":{"ponytail@ponytail":[{"installPath":"`+pon+`"}],"off@x":[{"installPath":"`+off+`"}]}}`)
	put(filepath.Join(proj, ".claude/skills/deploy/SKILL.md"), skill("deploy", "Ship it."))

	var got []string
	for _, s := range listSkills(home, proj) {
		got = append(got, s.Cmd+"|"+s.Src+"|"+s.Desc)
	}
	want := []string{
		"/deploy|project|Ship it.",
		"/cube-browser|yours|Use the browser.",
		"/standup|yours|Write my standup",
		"/ponytail:ponytail|ponytail|Forces the laziest solution that works.",
		"/pdf|claude.ai|Read PDFs.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("skills:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
