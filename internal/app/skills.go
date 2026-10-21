package app

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ninovee/usbai/internal/config"
)

// Skill packs (such as Claude Ads) are folders of SKILL.md instructions and
// reference files written for Claude Code. Agents read them with the skill
// tools, loading only the skill and references a task needs. Nothing in a
// pack runs.

type skillInfo struct {
	Name, Description string
	Dir               string // folder of SKILL.md, relative to the pack
	Pack              string // pack folder on the drive
}

// skillPacks returns the installed skill-pack apps.
func (a *App) skillPacks() []config.App {
	var out []config.App
	for _, c := range a.cfg.Apps {
		if c.Kind == "skills" && a.apps != nil && a.apps.Installed(c.ID) {
			out = append(out, c)
		}
	}
	return out
}

// skipSkillDir leaves out folders that hold tests and tooling, not skills.
var skipSkillDir = map[string]bool{".git": true, "node_modules": true, "tests": true, "evals": true, "home": true, "tmp": true}

func (a *App) listSkills() []skillInfo {
	var out []skillInfo
	for _, c := range a.skillPacks() {
		root := a.apps.Dir(c.ID)
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && skipSkillDir[d.Name()] {
				return filepath.SkipDir
			}
			if d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			name, desc := skillFrontMatter(p)
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			if name == "" {
				name = filepath.Base(filepath.Dir(p))
			}
			out = append(out, skillInfo{Name: name, Description: desc, Dir: filepath.ToSlash(rel), Pack: root})
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// skillFrontMatter reads name and description from a SKILL.md header.
func skillFrontMatter(p string) (name, desc string) {
	f, err := os.Open(p)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "description":
			desc = v
		}
	}
	return name, desc
}

func (a *App) skillTool(name string, arg func(string) string) (string, error) {
	skills := a.listSkills()
	if len(skills) == 0 {
		return "", errors.New("no skill pack is installed for this drive — install one in the Apps tab")
	}
	switch name {
	case "list_skills":
		var b strings.Builder
		for _, s := range skills {
			d := s.Description
			if r := []rune(d); len(r) > 240 {
				d = string(r[:240]) + "…"
			}
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, d)
		}
		return b.String(), nil
	case "read_skill":
		want := strings.TrimPrefix(strings.TrimSpace(arg("name")), "/")
		for _, s := range skills {
			if strings.EqualFold(s.Name, want) || strings.EqualFold(filepath.Base(s.Dir), want) {
				data, err := os.ReadFile(filepath.Join(s.Pack, filepath.FromSlash(s.Dir), "SKILL.md"))
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Skill %s (folder %s). Read the files it mentions with read_skill_file, using paths inside the pack (for example %s/SKILL.md).\n\n%s",
					s.Name, s.Dir, s.Dir, data), nil
			}
		}
		return "", fmt.Errorf("no skill named %q — call list_skills to see the names", want)
	case "read_skill_file":
		return readSkillFile(skills, arg("path"))
	}
	return "", errors.New("unknown skill tool")
}

// skillFileExts are the reference files agents may read.
var skillFileExts = map[string]bool{".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true, ".csv": true, ".toml": true, ".py": true}

const maxSkillFile = 512 << 10

// readSkillFile reads a text file inside a skill pack. A path that isn't
// found from the pack's top (skills often name files relative to their own
// folder) is looked up by its ending.
func readSkillFile(skills []skillInfo, rel string) (string, error) {
	rel = filepath.ToSlash(filepath.Clean(strings.TrimPrefix(strings.TrimSpace(rel), "/")))
	if rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
		return "", errors.New("give a path inside the skill pack, e.g. ads/references/scoring-system.md")
	}
	if !skillFileExts[strings.ToLower(filepath.Ext(rel))] {
		return "", errors.New("only text reference files can be read")
	}
	packs := map[string]bool{}
	for _, s := range skills {
		packs[s.Pack] = true
	}
	var found []string
	for pack := range packs {
		if p := filepath.Join(pack, filepath.FromSlash(rel)); isFile(p) {
			found = []string{p}
			break
		}
		filepath.WalkDir(pack, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && skipSkillDir[d.Name()] {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(filepath.ToSlash(p), "/"+rel) {
				found = append(found, p)
			}
			return nil
		})
	}
	switch {
	case len(found) == 0:
		return "", fmt.Errorf("no file %s in the skill pack", rel)
	case len(found) > 1:
		return "", fmt.Errorf("several files end with %s; give a longer path", rel)
	}
	st, err := os.Stat(found[0])
	if err != nil {
		return "", err
	}
	if st.Size() > maxSkillFile {
		return "", errors.New("that file is too large to read")
	}
	data, err := os.ReadFile(found[0])
	return string(data), err
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
