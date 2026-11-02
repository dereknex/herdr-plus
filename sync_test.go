//
// Date: 2026-10-04
// Author: Derek
//
// Tests for project auto-discovery (syncProjects). The "is this a git repo?"
// probe is injected so the scan runs without a git toolchain; the real probe
// (isInsideGitWorkTree) is exercised elsewhere.
//

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// repoProbe treats every directory as a git repo except one named "notrepo", so
// the scan's filtering can be asserted deterministically.
func repoProbe(path string) bool { return filepath.Base(path) != "notrepo" }

// scanFixture builds a temp XDG config root plus a scan dir holding the named
// child directories, and returns the root, the scan dir, and the projects dir.
func scanFixture(t *testing.T, children ...string) (root, projectsDir string) {
	t.Helper()
	tmp := t.TempDir()
	// Pin config to the temp XDG root even under a herdr plugin context.
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", tmp)

	root = filepath.Join(tmp, "workspaces")
	projectsDir = projectsDirIn(t, tmp)
	for _, name := range children {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	return root, projectsDir
}

// TestSyncProjectsCreatesForRepos is the happy path: each git-repo child without
// a project gets one, non-repos and dotdirs are skipped, the result loads, and a
// second run creates nothing (idempotent).
func TestSyncProjectsCreatesForRepos(t *testing.T) {
	root, projectsDir := scanFixture(t, "alpha", "beta", "notrepo", ".hidden")

	cfg := PluginConfig{}
	cfg.Projects.ScanDirs = []string{root}

	created, err := syncProjectsWith(cfg, repoProbe)
	if err != nil {
		t.Fatalf("syncProjectsWith: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created %v, want 2 files (alpha, beta)", created)
	}
	for _, name := range []string{"alpha.toml", "beta.toml"} {
		if _, err := os.Stat(filepath.Join(projectsDir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(projectsDir, "notrepo.toml")); !os.IsNotExist(err) {
		t.Fatal("notrepo should not have been synced")
	}

	projects, err := loadProjects()
	if err != nil {
		t.Fatalf("loadProjects: %v", err)
	}
	if len(projects) != 2 || projects[0].Name != "alpha" || projects[1].Name != "beta" {
		t.Fatalf("loaded %+v, want alpha then beta", projects)
	}
	if got := len(projects[0].Tabs); got != len(genericLayoutTabs()) {
		t.Fatalf("generated project has %d tabs, want %d", got, len(genericLayoutTabs()))
	}

	// Idempotent: the second pass finds everything covered.
	again, err := syncProjectsWith(cfg, repoProbe)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second sync created %v, want none", again)
	}
}

// TestSyncProjectsSkipsCoveredRepo confirms a repo already claimed by a project
// (matched by resolved working_dir, whatever the file is called) is not re-added.
func TestSyncProjectsSkipsCoveredRepo(t *testing.T) {
	root, projectsDir := scanFixture(t, "alpha", "beta")

	existing := `name = "Alpha Custom"
group = "Mine"
working_dir = "` + filepath.Join(root, "alpha") + `"

[[tabs]]
name = "terminal"
`
	if err := os.WriteFile(filepath.Join(projectsDir, "custom-name.toml"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	cfg := PluginConfig{}
	cfg.Projects.ScanDirs = []string{root}

	created, err := syncProjectsWith(cfg, repoProbe)
	if err != nil {
		t.Fatalf("syncProjectsWith: %v", err)
	}
	if len(created) != 1 || filepath.Base(created[0]) != "beta.toml" {
		t.Fatalf("created %v, want only beta.toml", created)
	}
	if _, err := os.Stat(filepath.Join(projectsDir, "alpha.toml")); !os.IsNotExist(err) {
		t.Fatal("alpha is already covered; alpha.toml should not have been created")
	}

	// The hand-written project is untouched (still named/grouped as authored).
	projects, err := loadProjects()
	if err != nil {
		t.Fatalf("loadProjects: %v", err)
	}
	for _, p := range projects {
		if p.Name == "Alpha Custom" && p.Group == "Mine" {
			return
		}
	}
	t.Fatal("the hand-written Alpha Custom project was not preserved")
}

// TestSyncProjectsNameCollision confirms that when a project file of the wanted
// name already exists (pointing elsewhere), sync leaves it alone rather than
// overwriting it, and still syncs the other repos.
func TestSyncProjectsNameCollision(t *testing.T) {
	root, projectsDir := scanFixture(t, "alpha", "beta")

	collide := `name = "Something Else"
working_dir = "/elsewhere/alpha"

[[tabs]]
name = "terminal"
`
	if err := os.WriteFile(filepath.Join(projectsDir, "alpha.toml"), []byte(collide), 0o644); err != nil {
		t.Fatalf("write colliding file: %v", err)
	}

	cfg := PluginConfig{}
	cfg.Projects.ScanDirs = []string{root}

	created, err := syncProjectsWith(cfg, repoProbe)
	if err != nil {
		t.Fatalf("syncProjectsWith: %v", err)
	}
	if len(created) != 1 || filepath.Base(created[0]) != "beta.toml" {
		t.Fatalf("created %v, want only beta.toml", created)
	}
	body, err := os.ReadFile(filepath.Join(projectsDir, "alpha.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Something Else") {
		t.Fatal("the colliding alpha.toml was overwritten")
	}
}

// TestRenderProjectTOMLRoundTrips confirms the generated file is valid TOML that
// decodes back into a valid project with the expected name and tabs, and that the
// keys are lower-cased as the hand-written files are.
func TestRenderProjectTOMLRoundTrips(t *testing.T) {
	body, err := renderProjectTOML("My Repo", "~/code/my-repo", genericLayoutTabs())
	if err != nil {
		t.Fatalf("renderProjectTOML: %v", err)
	}
	if !strings.Contains(body, "\nname = \"My Repo\"") {
		t.Fatalf("generated TOML is not lower-cased / tagged as expected:\n%s", body)
	}

	path := filepath.Join(t.TempDir(), "my-repo.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var p Project
	if _, err := toml.DecodeFile(path, &p); err != nil {
		t.Fatalf("decode generated TOML: %v\n%s", err, body)
	}
	if err := p.validate(); err != nil {
		t.Fatalf("generated project failed validation: %v", err)
	}
	if p.Name != "My Repo" || p.WorkingDir != "~/code/my-repo" {
		t.Fatalf("decoded %+v, want name/working_dir preserved", p)
	}
	if len(p.Tabs) != len(genericLayoutTabs()) {
		t.Fatalf("decoded %d tabs, want %d", len(p.Tabs), len(genericLayoutTabs()))
	}
}

// TestGenericLayoutTabsValid guards the built-in fallback layout: it must always
// pass the same validation a user's tabs do, since it is applied to worktrees and
// written into generated projects without a config file to blame.
func TestGenericLayoutTabsValid(t *testing.T) {
	if err := validateTabs("builtin", "(built-in default)", genericLayoutTabs()); err != nil {
		t.Fatalf("built-in generic layout is invalid: %v", err)
	}
}
