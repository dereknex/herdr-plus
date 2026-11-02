//
// Date: 2026-10-04
// Author: Derek
//
// herdr-plus project auto-discovery. A project is normally hand-written: one
// TOML file per workspace template. That is fine for a handful of repos but
// tedious once a machine holds dozens of them. syncProjects closes the gap: it
// scans the configured directories for git repos and writes a project file for
// every repo that does not already have one. It only ever *creates* — an
// existing file is never overwritten, so hand-tuned projects (groups, split
// panes, startup commands) survive a re-run untouched.
//
// Run it with `herdr-plus sync`, or let it happen for you: the projects browser
// syncs before it loads, so a freshly cloned repo appears without any manual
// step. Both paths share this one function.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// defaultScanDir is where syncProjects looks for repos when [projects] scan_dirs
// is unset. It is the directory the README's examples use, and the one a
// workspaces-per-repo layout naturally produces.
const defaultScanDir = "~/workspaces"

// syncProjects scans the configured directories for git repos and writes a
// project file for each one that has none. It returns the files it created.
//
// A repo is considered "already covered" when some existing project resolves to
// the same directory — so a project can be renamed, regrouped, or rewritten
// without sync trying to add a second file for it. A repo whose file name is
// already taken by a project pointing elsewhere is skipped with a warning rather
// than overwritten.
func syncProjects(cfg PluginConfig) ([]string, error) {
	return syncProjectsWith(cfg, isInsideGitWorkTree)
}

// syncProjectsWith is syncProjects with the "is this a git repo?" probe injected,
// so the scan can be unit-tested without a real git toolchain.
func syncProjectsWith(cfg PluginConfig, isRepo func(string) bool) ([]string, error) {
	dir, err := ensureProjectsDir()
	if err != nil {
		return nil, err
	}

	// Load what is already configured. A malformed project file fails the whole
	// load — deliberately, so sync never adds files on top of a config it cannot
	// read. The caller surfaces the error and the browser shows it.
	projects, err := loadProjects()
	if err != nil {
		return nil, err
	}
	covered := existingProjectDirs(projects)

	roots, err := scanDirs(cfg)
	if err != nil {
		return nil, err
	}

	var created []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue // a scan dir that does not exist contributes nothing
			}
			return created, fmt.Errorf("scan %s: %w", root, err)
		}
		for _, e := range entries {
			// Only immediate children, and nothing hidden (.git and friends).
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			repo, err := filepath.Abs(filepath.Join(root, e.Name()))
			if err != nil {
				continue
			}
			repo = filepath.Clean(repo)
			if covered[repo] || !isRepo(repo) {
				continue
			}

			target := filepath.Join(dir, e.Name()+".toml")
			if _, err := os.Stat(target); err == nil {
				fmt.Fprintf(os.Stderr, "herdr-plus: sync: %s already exists; leaving it alone (repo %s)\n", target, repo)
				continue
			} else if !os.IsNotExist(err) {
				return created, fmt.Errorf("stat %s: %w", target, err)
			}

			body, err := renderProjectTOML(e.Name(), tildePath(repo), genericLayoutTabs())
			if err != nil {
				return created, err
			}
			if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
				return created, fmt.Errorf("write %s: %w", target, err)
			}
			created = append(created, target)
		}
	}
	return created, nil
}

// scanDirs resolves the configured scan directories, falling back to
// defaultScanDir. ~ and $VARS expand exactly as they do in a project's
// working_dir.
func scanDirs(cfg PluginConfig) ([]string, error) {
	raw := cfg.Projects.ScanDirs
	if len(raw) == 0 {
		raw = []string{defaultScanDir}
	}
	var out []string
	for _, r := range raw {
		dir, err := expandPath(r)
		if err != nil {
			return nil, fmt.Errorf("resolve scan_dirs %q: %w", r, err)
		}
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out, nil
}

// existingProjectDirs returns the set of absolute directories already claimed by
// a project. Projects that prompt for their directory at open time ({prompt})
// have no fixed directory and are skipped.
func existingProjectDirs(projects []Project) map[string]bool {
	dirs := map[string]bool{}
	for _, p := range projects {
		if p.promptsForDir() {
			continue
		}
		dir, err := p.expandedWorkingDir()
		if err != nil || dir == "" {
			continue
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dirs[filepath.Clean(abs)] = true
		}
	}
	return dirs
}

// renderProjectTOML builds the file sync writes for a discovered repo: a short
// header explaining that sync owns only the creation, then the project itself,
// encoded by the TOML library so the values are always escaped correctly. The
// tabs are the built-in generic layout, which is also what a worktree falls back
// to — one definition of "the default workspace" for both features.
func renderProjectTOML(name, workingDir string, tabs []ProjectTab) (string, error) {
	var b strings.Builder
	b.WriteString("# Generated by `herdr-plus sync` from a git repo under your scan_dirs.\n")
	b.WriteString("# Edit freely — sync never overwrites an existing project file.\n")
	b.WriteString("# Add `group = \"...\"` to cluster this project under a heading in the browser.\n\n")

	if err := toml.NewEncoder(&b).Encode(Project{Name: name, WorkingDir: workingDir, Tabs: tabs}); err != nil {
		return "", fmt.Errorf("encode project for %s: %w", name, err)
	}
	return b.String(), nil
}

// tildePath rewrites an absolute path under the home directory back to a ~ form,
// so a generated working_dir reads like the hand-written ones. Paths outside home
// (or when home cannot be determined) are returned unchanged.
func tildePath(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if strings.HasPrefix(dir, home+string(filepath.Separator)) {
		return "~" + dir[len(home):]
	}
	return dir
}

// runSync implements the `herdr-plus sync` subcommand: the explicit, scriptable
// entry point for project auto-discovery. Like the other direct commands it asks
// the running herdr where the managed config lives, so it reads and writes the
// same projects/ the picker does.
func runSync() {
	ensureManagedConfigDir()

	cfg, err := loadPluginConfig()
	if err != nil {
		errExit(err)
	}

	created, err := syncProjects(cfg)
	if err != nil {
		errExit(err)
	}
	if len(created) == 0 {
		fmt.Println("herdr-plus: sync: no new projects.")
		return
	}
	for _, path := range created {
		fmt.Println("added", path)
	}
}
