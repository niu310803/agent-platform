package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/api"
)

func TestSkillPackageManifestDeclaredMembersAndConditionalEdit(t *testing.T) {
	f := newTestFixture(t)
	archive := serverSkillImportZIP(t, map[string]string{
		"package.json":  `{"name":"sample-suite","skills":[{"key":"same"}]}`,
		"same/SKILL.md": "---\nname: same\ndescription: member\n---\nMember body.\n",
	})
	request := httptest.NewRequest(http.MethodPost, "/api/admin/skill-packages/import?key=sample-suite", bytes.NewReader(archive))
	request.Header.Set("Content-Type", "application/zip")
	response := httptest.NewRecorder()
	f.server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("minimal manifest import: %d %s", response.Code, response.Body.String())
	}
	packages := getAPIData[[]api.AdminSkillPackageResponse](t, f.server, "GET", "/api/admin/skill-packages", nil)
	if len(packages) != 1 || packages[0].DisplayName != "sample-suite" || packages[0].Version != "" || len(packages[0].Skills) != 1 || packages[0].Skills[0].ID != "sample-suite/same" {
		t.Fatalf("packages=%+v", packages)
	}
	file := getAPIData[skillPackageManifestResponse](t, f.server, "GET", "/api/admin/skill-packages/manifest?key=sample-suite", nil)
	body, _ := json.Marshal(map[string]string{"key": "sample-suite", "content": `{"name":"sample-suite","displayName":"测试技能包","skills":[{"key":"same"}]}`, "baseSha256": file.SHA256})
	saved := getAPIData[skillPackageManifestResponse](t, f.server, "PUT", "/api/admin/skill-packages/manifest", body)
	if saved.SHA256 == file.SHA256 {
		t.Fatal("edit did not update revision")
	}
	packages = getAPIData[[]api.AdminSkillPackageResponse](t, f.server, "GET", "/api/admin/skill-packages", nil)
	if packages[0].DisplayName != "测试技能包" {
		t.Fatalf("displayName not updated: %+v", packages)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/admin/skill-packages/manifest", bytes.NewReader(body))
	response = httptest.NewRecorder()
	f.server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale edit expected 409: %d %s", response.Code, response.Body.String())
	}
	disk, err := os.ReadFile(filepath.Join(f.cfg.Paths.SkillsCenterDir, "sample-suite", "package.json"))
	if err != nil || !bytes.Contains(disk, []byte(`"skills"`)) {
		t.Fatalf("explicit member list must be persisted: %s %v", disk, err)
	}
}

func TestSkillPackageManifestEditReloadFailureRestoresOriginal(t *testing.T) {
	f := newAgentSkillsTestFixture(t, false)
	writeProjectionPackage(t, f, "center-extra")
	original := getAPIData[skillPackageManifestResponse](t, f.server, "GET", "/api/admin/skill-packages/manifest?key=office", nil)
	f.server.deps.CatalogReloader = failingSkillImportReloader{}
	body, _ := json.Marshal(map[string]string{"key": "office", "content": `{"name":"office","displayName":"must rollback","skills":[{"key":"center-extra"}]}`, "baseSha256": original.SHA256})
	request := httptest.NewRequest(http.MethodPut, "/api/admin/skill-packages/manifest", bytes.NewReader(body))
	response := httptest.NewRecorder()
	f.server.ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("expected reload failure")
	}
	after := getAPIData[skillPackageManifestResponse](t, f.server, "GET", "/api/admin/skill-packages/manifest?key=office", nil)
	if after != original {
		t.Fatalf("manifest not rolled back: %+v", after)
	}
}

func TestSkillPackageAndStandaloneUpdatesAreIndependent(t *testing.T) {
	f := newTestFixture(t)
	standaloneDir := filepath.Join(f.cfg.Paths.SkillsCenterDir, "shared-name")
	writeTestSkill(t, f.cfg.Paths.SkillsCenterDir, "shared-name")
	standaloneBefore, err := os.ReadFile(filepath.Join(standaloneDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	importPackage := func(version string) {
		t.Helper()
		content := "---\nname: shared-name\ndescription: Package " + version + "\n---\nPackage body " + version
		archive := serverSkillImportZIP(t, map[string]string{"package.json": `{"name":"independent-suite","version":"` + version + `","skills":[{"key":"shared-name"}]}`, "shared-name/SKILL.md": content})
		req := httptest.NewRequest(http.MethodPost, "/api/admin/skill-packages/import?key=independent-suite", bytes.NewReader(archive))
		req.Header.Set("Content-Type", "application/zip")
		rec := httptest.NewRecorder()
		f.server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("package install %s: %d %s", version, rec.Code, rec.Body.String())
		}
	}
	importPackage("1.0.0")
	importPackage("2.0.0")
	standaloneAfter, _ := os.ReadFile(filepath.Join(standaloneDir, "SKILL.md"))
	if !bytes.Equal(standaloneBefore, standaloneAfter) {
		t.Fatal("package update changed standalone skill")
	}
	memberPath := filepath.Join(f.cfg.Paths.SkillsCenterDir, "independent-suite", "shared-name", "SKILL.md")
	memberBefore, err := os.ReadFile(memberPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := serverSkillImportZIP(t, map[string]string{"SKILL.md": "---\nname: shared-name\ndescription: Standalone update\n---\nNew standalone content"})
	body, contentType := skillImportBody(t, "shared-name", "shared-name.zip", archive)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/skills/import?overwrite=true", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("standalone update: %d %s", rec.Code, rec.Body.String())
	}
	memberAfter, _ := os.ReadFile(memberPath)
	if !bytes.Equal(memberBefore, memberAfter) {
		t.Fatal("standalone update changed package member")
	}
	standaloneAfter, _ = os.ReadFile(filepath.Join(standaloneDir, "SKILL.md"))
	if !bytes.Contains(standaloneAfter, []byte("New standalone content")) {
		t.Fatal("standalone was not updated")
	}
	getAPIData[api.DeleteAdminSkillPackageResponse](t, f.server, "POST", "/api/admin/skill-packages/delete", []byte(`{"key":"independent-suite"}`))
	remaining, _ := os.ReadFile(filepath.Join(standaloneDir, "SKILL.md"))
	if !bytes.Equal(standaloneAfter, remaining) {
		t.Fatal("package deletion changed standalone skill")
	}
}

func TestStandaloneSkillUninstallLeavesPackageMemberAndUpdateIndependent(t *testing.T) {
	f := newAgentSkillsTestFixture(t, false)
	writeProjectionPackage(t, f, "center-extra")
	memberPath := filepath.Join(f.cfg.Paths.SkillsCenterDir, "office", "center-extra", "SKILL.md")
	before, err := os.ReadFile(memberPath)
	if err != nil {
		t.Fatal(err)
	}
	deleted := getAPIData[api.DeleteAdminSkillResponse](t, f.server, http.MethodPost, "/api/admin/skills/delete", []byte(`{"key":"center-extra"}`))
	if !deleted.Deleted {
		t.Fatal("standalone was not deleted")
	}
	after, err := os.ReadFile(memberPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("standalone deletion changed package member: %v", err)
	}
	packages := getAPIData[[]api.AdminSkillPackageResponse](t, f.server, http.MethodGet, "/api/admin/skill-packages", nil)
	if len(packages) != 1 || len(packages[0].Skills) != 1 || packages[0].Skills[0].ID != "office/center-extra" {
		t.Fatalf("package changed: %+v", packages)
	}
	archive := serverSkillImportZIP(t, map[string]string{
		"package.json":          `{"name":"office","version":"2","skills":[{"key":"center-extra"}]}`,
		"center-extra/SKILL.md": "---\nname: center-extra\ndescription: Updated package member\n---\nUpdated package content",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/skill-packages/import?key=office", bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/zip")
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("package update: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Paths.SkillsCenterDir, "center-extra")); !os.IsNotExist(err) {
		t.Fatalf("package update recreated uninstalled standalone skill: %v", err)
	}
	after, err = os.ReadFile(memberPath)
	if err != nil || !bytes.Contains(after, []byte("Updated package content")) {
		t.Fatalf("package member not updated: %v", err)
	}
}

func TestSkillPackageDeclareMissingMemberThenCreateThroughAPI(t *testing.T) {
	f := newAgentSkillsTestFixture(t, false)
	writeProjectionPackage(t, f, "center-extra")
	createBody := mustSkillJSON(t, api.CreateAdminSkillRequest{Key: "office/new-member", SkillMd: "---\nname: new-member\ndescription: New member\n---\nNew member body"})
	request := httptest.NewRequest(http.MethodPost, "/api/admin/skills/create", bytes.NewReader(createBody))
	response := httptest.NewRecorder()
	f.server.ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("undeclared member creation succeeded")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Paths.SkillsCenterDir, "office", "new-member")); !os.IsNotExist(err) {
		t.Fatalf("undeclared member created on disk: %v", err)
	}

	original := getAPIData[skillPackageManifestResponse](t, f.server, http.MethodGet, "/api/admin/skill-packages/manifest?key=office", nil)
	content := `{"name":"office","skills":[{"key":"center-extra"},{"key":"new-member"}]}`
	body, _ := json.Marshal(map[string]string{"key": "office", "content": content, "baseSha256": original.SHA256})
	getAPIData[skillPackageManifestResponse](t, f.server, http.MethodPut, "/api/admin/skill-packages/manifest", body)
	packages := getAPIData[[]api.AdminSkillPackageResponse](t, f.server, http.MethodGet, "/api/admin/skill-packages", nil)
	if len(packages) != 1 || packages[0].Status != "incomplete" || len(packages[0].Skills) != 2 || len(packages[0].MissingSkillIDs) != 1 || packages[0].MissingSkillIDs[0] != "office/new-member" {
		t.Fatalf("missing declaration not retained: %+v", packages)
	}
	if packages[0].Skills[1].Key != "office/new-member" || packages[0].Skills[1].ID != "office/new-member" {
		t.Fatalf("qualified identity missing: %+v", packages[0].Skills[1])
	}
	chat := getAPIData[api.AgentSkillsResponse](t, f.server, http.MethodGet, "/api/skills", nil)
	if len(chat.Packages) != 1 || len(chat.Packages[0].Skills) != 1 {
		t.Fatalf("chat exposed missing member: %+v", chat.Packages)
	}
	created := getAPIData[api.AdminSkillDetailResponse](t, f.server, http.MethodPost, "/api/admin/skills/create", createBody)
	if created.Skill.Key != "office/new-member" || created.Skill.Status != "ready" {
		t.Fatalf("repair failed: %+v", created)
	}
	packages = getAPIData[[]api.AdminSkillPackageResponse](t, f.server, http.MethodGet, "/api/admin/skill-packages", nil)
	if packages[0].Status != "ready" || len(packages[0].MissingSkillIDs) != 0 {
		t.Fatalf("repaired package remains incomplete: %+v", packages)
	}
	chat = getAPIData[api.AgentSkillsResponse](t, f.server, http.MethodGet, "/api/skills", nil)
	if len(chat.Packages) != 1 || len(chat.Packages[0].Skills) != 2 {
		t.Fatalf("chat missing repaired member: %+v", chat.Packages)
	}
}
