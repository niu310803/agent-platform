package catalog

import (
	"agent-platform/internal/config"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitPackageManifestValidation(t *testing.T) {
	for _, raw := range []string{`{"name":"suite"}`, `{"name":"suite","skills":null}`, `{"name":"suite","skills":{}}`, `{"name":"suite","skills":[{}]}`, `{"name":"suite","skills":[{"key":"../outside"}]}`, `{"name":"suite","skills":[{"key":"A"},{"key":"a"}]}`} {
		if _, err := parseSkillPackageMetadata([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := parseSkillPackageMetadata([]byte(`{"name":"suite","skills":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitMembersMissingAndUnlisted(t *testing.T) {
	root := t.TempDir()
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "package.json"), `{"name":"suite","skills":[{"key":"missing"}]}`)
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "extra", "SKILL.md"), "# extra")
	r := &FileRegistry{cfg: config.Config{Paths: config.PathsConfig{SkillsCenterDir: root}}}
	record, err := ScanSkillPackageRecord(root, "suite")
	if err != nil || len(record.Skills) != 1 || record.Skills[0].ID != "suite/missing" || len(record.Skills[0].Diagnostics) != 1 {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	current, err := r.ReadEditableSkillPackageManifest("suite")
	if err != nil {
		t.Fatal(err)
	}
	staged, pending, err := r.BeginUpdateEditableSkillPackageManifest("suite", current.Content, current.SHA256)
	if err != nil || len(pending.Skills) != 1 || len(pending.Skills[0].Diagnostics) == 0 {
		t.Fatalf("missing declaration not retained: %+v %v", pending, err)
	}
	if err := staged.Rollback(); err != nil {
		t.Fatal(err)
	}
	archive := nestedPackageZIP(t, map[string]string{"package.json": current.Content})
	if _, _, err := r.BeginImportEditableSkillPackageArchive("suite", "", bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("imported missing member")
	}
	m, record, err := r.BeginUpdateEditableSkillPackageManifest("suite", `{"name":"suite","skills":[]}`, current.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(record.Skills) != 0 {
		t.Fatal(record)
	}
	if _, err = os.Stat(filepath.Join(root, "suite", "extra", "SKILL.md")); err != nil {
		t.Fatal("manifest save removed unlisted files", err)
	}
}

func TestExplicitMemberDeleteRollbackAndEmptyPackage(t *testing.T) {
	root := t.TempDir()
	original := `{"name":"suite","skills":[{"key":"member"}],"custom":true}`
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "package.json"), original)
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "member", "SKILL.md"), "# member")
	r := &FileRegistry{cfg: config.Config{Paths: config.PathsConfig{SkillsCenterDir: root}}}
	m, record, deleted, err := r.BeginDeleteEditableSkillPackageSkill("suite", "suite/member")
	if err != nil || deleted || len(record.Skills) != 0 {
		t.Fatalf("%+v %v %v", record, deleted, err)
	}
	manifest, err := ReadSkillPackageManifest(filepath.Join(root, "suite"))
	if err != nil || len(manifest.Skills) != 0 {
		t.Fatal(manifest, err)
	}
	if err = m.Rollback(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "suite", "package.json"))
	if string(raw) != original {
		t.Fatal("manifest rollback mismatch")
	}
	if _, err = os.Stat(filepath.Join(root, "suite", "member", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	m, _, _, err = r.BeginDeleteEditableSkillPackageSkill("suite", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Commit(); err != nil {
		t.Fatal(err)
	}
	manifest, err = ReadSkillPackageManifest(filepath.Join(root, "suite"))
	if err != nil || len(manifest.Skills) != 0 {
		t.Fatal(manifest, err)
	}
}

func TestImplicitPackageMigrationIsExplicitAndBackedUp(t *testing.T) {
	root := t.TempDir()
	original := `{"name":"suite","custom":{"preserve":true}}`
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "package.json"), original)
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "member", "SKILL.md"), "# member")
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "docs", "readme.md"), "support")
	if _, err := ReadSkillPackageManifest(filepath.Join(root, "suite")); err == nil {
		t.Fatal("strict read accepted implicit package")
	}
	r := &FileRegistry{cfg: config.Config{Paths: config.PathsConfig{SkillsCenterDir: root}}}
	backups, err := r.MigrateLegacySkillPackages()
	if err != nil || len(backups) != 1 {
		t.Fatal(backups, err)
	}
	raw, err := os.ReadFile(filepath.Join(backups[0], "suite.package.json"))
	if err != nil || string(raw) != original {
		t.Fatal("backup mismatch", err)
	}
	manifest, err := ReadSkillPackageManifest(filepath.Join(root, "suite"))
	if err != nil || len(manifest.Skills) != 1 || manifest.Skills[0].Key != "member" {
		t.Fatal(manifest, err)
	}
	upgraded, err := os.ReadFile(filepath.Join(root, "suite", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(upgraded, &fields); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(fields["custom"], []byte("true")) {
		t.Fatalf("migration lost extensions: %s", upgraded)
	}
	backups, err = r.MigrateLegacySkillPackages()
	if err != nil || len(backups) != 0 {
		t.Fatal("migration repeated", backups, err)
	}
}

func TestManifestRemovingMemberPreservesItsFiles(t *testing.T) {
	root := t.TempDir()
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "package.json"), `{"name":"suite","skills":[{"key":"member"}]}`)
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "member", "SKILL.md"), "# member")
	r := &FileRegistry{cfg: config.Config{Paths: config.PathsConfig{SkillsCenterDir: root}}}
	current, err := r.ReadEditableSkillPackageManifest("suite")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.BeginUpdateEditableSkillPackageManifest("suite", `{"name":"renamed","skills":[]}`, current.SHA256); err == nil {
		t.Fatal("renamed immutable identity")
	}
	m, record, err := r.BeginUpdateEditableSkillPackageManifest("suite", `{"name":"suite","skills":[]}`, current.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(record.Skills) != 0 {
		t.Fatal(record)
	}
	if _, err = os.Stat(filepath.Join(root, "suite", "member", "SKILL.md")); err != nil {
		t.Fatal("removed retained member files", err)
	}
}

func TestManifestRegisterMissingMemberThenCreate(t *testing.T) {
	root := t.TempDir()
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "package.json"), `{"name":"suite","skills":[]}`)
	r := &FileRegistry{cfg: config.Config{Paths: config.PathsConfig{SkillsCenterDir: root}}}
	current, err := r.ReadEditableSkillPackageManifest("suite")
	if err != nil {
		t.Fatal(err)
	}
	m, record, err := r.BeginUpdateEditableSkillPackageManifest("suite", `{"name":"suite","skills":[{"key":"member"}]}`, current.SHA256)
	if err != nil || len(record.Skills) != 1 || len(record.Skills[0].Diagnostics) == 0 {
		t.Fatalf("%+v %v", record, err)
	}
	if err = m.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.CreateEditableSkill("suite/member", "---\nname: member\n---\n# Member", nil); err != nil {
		t.Fatal(err)
	}
	record, err = ScanSkillPackageRecord(root, "suite")
	if err != nil || len(record.Skills) != 1 || len(record.Skills[0].Diagnostics) != 0 {
		t.Fatalf("%+v %v", record, err)
	}
}

func TestManifestCannotRemoveMemberUsedByAgent(t *testing.T) {
	root := t.TempDir()
	original := `{"name":"suite","skills":[{"key":"member"}]}`
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "package.json"), original)
	writeRuntimeAssemblerFile(t, filepath.Join(root, "suite", "member", "SKILL.md"), "# member")
	r := &FileRegistry{cfg: config.Config{Paths: config.PathsConfig{SkillsCenterDir: root}}, agents: map[string]AgentDefinition{"writer": {Key: "writer", Skills: []string{"suite/member"}}}}
	current, err := r.ReadEditableSkillPackageManifest("suite")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.BeginUpdateEditableSkillPackageManifest("suite", `{"name":"suite","skills":[]}`, current.SHA256); !errors.Is(err, ErrSkillPackageConflict) {
		t.Fatalf("used member removed: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "suite", "package.json"))
	if string(raw) != original {
		t.Fatal("rejected edit changed manifest")
	}
	if _, err = os.Stat(filepath.Join(root, "suite", "member", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestPackageManifestExtensionsSurviveImportSaveAndMemberDelete(t *testing.T) {
	root := t.TempDir()
	r := &FileRegistry{cfg: config.Config{Paths: config.PathsConfig{SkillsCenterDir: root}}}
	raw := `{"name":"suite","author":{"id":9007199254740993},"skills":[{"key":"first","custom":{"label":"keep","count":9007199254740993}},{"key":"second","custom":false}]}`
	archive := nestedPackageZIP(t, map[string]string{"package.json": raw, "first/SKILL.md": "# First", "second/SKILL.md": "# Second"})
	imported, _, err := r.BeginImportEditableSkillPackageArchive("suite", "", bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	if err = imported.Commit(); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "suite", "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		var members []map[string]json.RawMessage
		if err = json.Unmarshal(fields["skills"], &members); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(fields["author"], []byte("9007199254740993")) || len(members) == 0 || !bytes.Contains(members[0]["custom"], []byte("9007199254740993")) {
			t.Fatalf("extensions lost: %s", data)
		}
	}
	check()
	current, err := r.ReadEditableSkillPackageManifest("suite")
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := r.BeginUpdateEditableSkillPackageManifest("suite", current.Content, current.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err = saved.Commit(); err != nil {
		t.Fatal(err)
	}
	check()
	removed, _, _, err := r.BeginDeleteEditableSkillPackageSkill("suite", "second")
	if err != nil {
		t.Fatal(err)
	}
	if err = removed.Commit(); err != nil {
		t.Fatal(err)
	}
	check()
}
