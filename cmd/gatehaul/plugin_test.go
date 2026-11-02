package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The repository is also a Claude Code plugin and marketplace. These tests keep
// its manifests and its skill consistent with the CLI.

func readRepoFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPluginManifests(t *testing.T) {
	var plugin struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		License string `json:"license"`
	}
	if err := json.Unmarshal(readRepoFile(t, ".claude-plugin/plugin.json"), &plugin); err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	if plugin.Name != "gatehaul" || plugin.Version == "" || plugin.License != "Apache-2.0" {
		t.Errorf("plugin.json: name %q, version %q, license %q", plugin.Name, plugin.Version, plugin.License)
	}

	var market struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(readRepoFile(t, ".claude-plugin/marketplace.json"), &market); err != nil {
		t.Fatalf("marketplace.json: %v", err)
	}
	if market.Name != "gatehaul" || len(market.Plugins) != 1 || market.Plugins[0].Name != "gatehaul" || market.Plugins[0].Source != "./" {
		t.Errorf("marketplace.json must list the plugin at the repository root: %+v", market)
	}
}

func TestSkillFrontmatter(t *testing.T) {
	text := string(readRepoFile(t, "skills/gatehaul/SKILL.md"))
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		t.Fatal("SKILL.md must start with a YAML frontmatter")
	}
	front, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(front, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("frontmatter line %q is not key: value", line)
		}
		fields[key] = value
	}
	// Keys outside this set are refused when the skill is uploaded outside
	// Claude Code.
	portable := map[string]bool{"name": true, "description": true, "license": true, "compatibility": true, "metadata": true, "allowed-tools": true}
	for key := range fields {
		if !portable[key] {
			t.Errorf("frontmatter key %q is not portable", key)
		}
	}
	if fields["name"] != "gatehaul" {
		t.Errorf("name = %q, want gatehaul", fields["name"])
	}
	if d := fields["description"]; d == "" || len(d) > 1536 {
		t.Errorf("description must be set and at most 1536 characters, has %d", len(d))
	}
}

// TestSkillUsesOnlyImplementedCommands keeps the skill from teaching a command
// that the CLI does not have yet.
func TestSkillUsesOnlyImplementedCommands(t *testing.T) {
	text := string(readRepoFile(t, "skills/gatehaul/SKILL.md"))
	implemented := map[string]bool{"doctor": true, "version": true, "help": true, "up": true, "down": true, "run": true, "gen": true, "apply": true, "wait": true, "log": true}
	for _, m := range regexp.MustCompile("`gatehaul ([a-z]+)").FindAllStringSubmatch(text, -1) {
		if planned[m[1]] || !implemented[m[1]] {
			t.Errorf("SKILL.md uses gatehaul %s, which is not implemented", m[1])
		}
	}
}
