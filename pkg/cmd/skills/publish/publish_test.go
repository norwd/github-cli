package publish

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/git"
	"github.com/cli/cli/v2/internal/prompter"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/httpmock"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initGitRepo initializes a git repo in the given directory and adds remotes.
// Use this when the git repo must live in the same directory as the skill files.
// A local bare repo is created as the push target so that ensurePushed can work
// during publish tests, while the fetch URL remains the GitHub URL so that
// detectGitHubRemote still resolves the correct owner/repo.
func initGitRepo(t *testing.T, dir string, remoteURLs map[string]string) {
	t.Helper()

	bareDir := filepath.Join(t.TempDir(), "upstream.git")
	require.NoError(t, os.MkdirAll(bareDir, 0o755))
	runGitInDir(t, bareDir, "init", "--bare", "--initial-branch=main")

	runGitInDir(t, dir, "init", "--initial-branch=main")
	runGitInDir(t, dir, "config", "user.email", "monalisa@github.com")
	runGitInDir(t, dir, "config", "user.name", "Monalisa Octocat")
	for name, url := range remoteURLs {
		runGitInDir(t, dir, "remote", "add", name, url)
		runGitInDir(t, dir, "remote", "set-url", "--push", name, bareDir)
	}

	runGitInDir(t, dir, "add", ".")
	runGitInDir(t, dir, "commit", "--allow-empty", "-m", "init")
	if _, ok := remoteURLs["origin"]; ok {
		runGitInDir(t, dir, "push", "origin", "main")
	}
}

// stubAllSecureRemote registers the standard stubs for a fully-configured remote
// repo (topics, tags, rulesets, security) so publishRun skips all remote warnings.
func stubAllSecureRemote(reg *httpmock.Registry, owner, repo string) {
	reg.Register(
		httpmock.REST("GET", "repos/"+owner+"/"+repo+"/topics"),
		httpmock.JSONResponse(map[string]interface{}{
			"names": []string{"agent-skills"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", "repos/"+owner+"/"+repo+"/tags"),
		httpmock.JSONResponse([]map[string]interface{}{
			{"name": "v1.0.0"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", "repos/"+owner+"/"+repo+"/rulesets"),
		httpmock.JSONResponse([]map[string]interface{}{
			{"id": 1, "name": "tags", "target": "tag", "enforcement": "active"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", "repos/"+owner+"/"+repo),
		httpmock.JSONResponse(map[string]interface{}{
			"security_and_analysis": map[string]interface{}{
				"secret_scanning":                 map[string]interface{}{"status": "enabled"},
				"secret_scanning_push_protection": map[string]interface{}{"status": "enabled"},
			},
		}),
	)
}

func TestNewCmdPublish(t *testing.T) {
	tests := []struct {
		name      string
		cli       string
		wantsErr  bool
		wantsOpts PublishOptions
	}{
		{
			name: "all flags",
			cli:  "./monalisa-skills --dry-run --fix --tag v1.0.0",
			wantsOpts: PublishOptions{
				Dir:    "./monalisa-skills",
				DryRun: true,
				Fix:    true,
				Tag:    "v1.0.0",
			},
		},
		{
			name: "directory only",
			cli:  "./octocat-repo",
			wantsOpts: PublishOptions{
				Dir: "./octocat-repo",
			},
		},
		{
			name:      "no args leaves dir empty",
			cli:       "",
			wantsOpts: PublishOptions{},
		},
		{
			name: "dry-run flag only",
			cli:  "--dry-run",
			wantsOpts: PublishOptions{
				DryRun: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ios, _, _, _ := iostreams.Test()
			f := cmdutil.Factory{IOStreams: ios}

			var gotOpts *PublishOptions
			cmd := NewCmdPublish(&f, func(opts *PublishOptions) error {
				gotOpts = opts
				return nil
			})

			args, err := shlex.Split(tt.cli)
			require.NoError(t, err)
			cmd.SetArgs(args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})

			err = cmd.Execute()
			if tt.wantsErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, gotOpts)
			assert.Equal(t, tt.wantsOpts.Dir, gotOpts.Dir)
			assert.Equal(t, tt.wantsOpts.DryRun, gotOpts.DryRun)
			assert.Equal(t, tt.wantsOpts.Fix, gotOpts.Fix)
			assert.Equal(t, tt.wantsOpts.Tag, gotOpts.Tag)
		})
	}
}

func TestPublishRun_UnsupportedHost(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "test-skill", heredoc.Doc(`
		---
		name: test-skill
		description: A test skill
		---
		Body.
	`))

	ios, _, _, _ := iostreams.Test()
	initGitRepo(t, dir, map[string]string{"origin": "https://github.com/monalisa/skills-repo.git"})
	err := publishRun(&PublishOptions{
		IO:        ios,
		Dir:       dir,
		GitClient: &git.Client{},
		client:    api.NewClientFromHTTP(&http.Client{}),
		host:      "acme.ghes.com",
	})
	require.ErrorContains(t, err, "supports only github.com")
}

func TestPublishRun(t *testing.T) {
	tests := []struct {
		name       string
		isTTY      bool
		setup      func(t *testing.T, dir string)
		stubs      func(*httpmock.Registry)
		opts       func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions
		verify     func(t *testing.T, dir string)
		wantErr    string
		wantStdout string
		wantStderr string
	}{
		{
			name:  "no skills found",
			setup: func(_ *testing.T, _ string) {},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir}
			},
			wantErr: "no skills found",
		},
		{
			name: "empty skills directory has no discoverable skills",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "skills", "empty-skill"), 0o755))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir}
			},
			wantErr: "no skills found",
		},
		{
			name: "missing name in frontmatter",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "git-commit", heredoc.Doc(`
					---
					description: A skill for writing good git commits
					---
					Body text.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir}
			},
			wantErr:    "validation failed",
			wantStdout: "missing required field: name",
		},
		{
			name: "name does not match directory",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "git-commit", heredoc.Doc(`
					---
					name: wrong-name
					description: A skill
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir}
			},
			wantErr:    "validation failed",
			wantStdout: "does not match directory name",
		},
		{
			name: "non-spec-compliant name",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "My_Skill", heredoc.Doc(`
					---
					name: My_Skill
					description: A skill with non-compliant name
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir}
			},
			wantErr:    "validation failed",
			wantStdout: "naming convention",
		},
		{
			name:  "root-level skill discovered and validated",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// Create a root-level skill (*/SKILL.md convention)
				skillDir := filepath.Join(dir, "my-root-skill")
				require.NoError(t, os.MkdirAll(skillDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(heredoc.Doc(`
					---
					name: my-root-skill
					description: A root-level skill
					license: MIT
					---
					Body.
				`)), 0o644))
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					DryRun:    true,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "1 skill(s) validated successfully",
			wantStderr: "Dry run complete",
		},
		{
			name:  "namespaced skill discovered and validated",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// Create a namespaced skill (skills/{scope}/*/SKILL.md convention)
				skillDir := filepath.Join(dir, "skills", "monalisa", "scoped-skill")
				require.NoError(t, os.MkdirAll(skillDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(heredoc.Doc(`
					---
					name: scoped-skill
					description: A namespaced skill
					license: MIT
					---
					Body.
				`)), 0o644))
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					DryRun:    true,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "1 skill(s) validated successfully",
			wantStderr: "Dry run complete",
		},
		{
			name:  "valid skill dry-run passes validation",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "good-skill", heredoc.Doc(`
					---
					name: good-skill
					description: A good skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					DryRun:    true,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "1 skill(s) validated successfully",
			wantStderr: "Dry run complete",
		},
		{
			name:  "valid skill with --tag publishes release",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "git-commit", heredoc.Doc(`
					---
					name: git-commit
					description: A skill for writing good git commits
					allowed-tools: git
					license: MIT
					---
					You are a git commit expert.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
				// topic already present, so no PUT needed
				// immutable releases check
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.JSONResponse(map[string]interface{}{"enabled": true}),
				)
				// default branch for branch comparison
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{"default_branch": "main"}),
				)
				// create release
				reg.Register(
					httpmock.REST("POST", "repos/monalisa/skills-repo/releases"),
					httpmock.JSONResponse(map[string]interface{}{
						"html_url": "https://github.com/monalisa/skills-repo/releases/tag/v1.0.1",
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:  ios,
					Dir: dir,
					Tag: "v1.0.1",
					Prompter: &prompter.PrompterMock{
						ConfirmFunc: func(msg string, def bool) (bool, error) { return true, nil },
					},
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "Published v1.0.1",
		},
		{
			name: "strip metadata with --fix",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "test-skill", heredoc.Doc(`
					---
					name: test-skill
					description: A test skill
					metadata:
					    github-owner: someone
					    github-repo: something
					    github-ref: v1.0.0
					    github-sha: abc123
					    github-tree-sha: def456
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir, Fix: true}
			},
			wantStdout: "stripped install metadata",
			verify: func(t *testing.T, dir string) {
				t.Helper()
				fixed, err := os.ReadFile(filepath.Join(dir, "skills", "test-skill", "SKILL.md"))
				require.NoError(t, err)
				fixedStr := string(fixed)
				assert.NotContains(t, fixedStr, "github-owner")
				assert.NotContains(t, fixedStr, "github-sha")
				assert.NotContains(t, fixedStr, "metadata:")
			},
		},
		{
			name: "metadata without --fix errors with hint",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "test-skill", heredoc.Doc(`
					---
					name: test-skill
					description: A test skill
					metadata:
					    github-owner: someone
					    github-sha: abc123
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir, Fix: false}
			},
			wantErr:    "validation failed",
			wantStdout: "--fix",
		},
		{
			name: "missing license warning",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "no-license", heredoc.Doc(`
					---
					name: no-license
					description: A skill without license
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir}
			},
			wantStdout: "license",
		},
		{
			name: "allowed-tools array error",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "bad-tools", heredoc.Doc(`
					---
					name: bad-tools
					description: A skill with array allowed-tools
					allowed-tools:
					  - git
					  - curl
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{IO: ios, Dir: dir}
			},
			wantErr:    "validation failed",
			wantStdout: "allowed-tools must be a string",
		},
		{
			name: "security warnings when features disabled",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/octocat/secure-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{
						"names": []string{"agent-skills"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/secure-repo/tags"),
					httpmock.JSONResponse([]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/secure-repo/rulesets"),
					httpmock.JSONResponse([]map[string]interface{}{
						{"id": 1, "name": "branch-only", "target": "branch", "enforcement": "active"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/secure-repo"),
					httpmock.JSONResponse(map[string]interface{}{
						"security_and_analysis": map[string]interface{}{
							"secret_scanning":                 map[string]interface{}{"status": "disabled"},
							"secret_scanning_push_protection": map[string]interface{}{"status": "disabled"},
						},
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/octocat/secure-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "secret scanning is not enabled",
		},
		{
			name: "tag protection warning",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/octocat/tag-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{
						"names": []string{"agent-skills"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/tag-repo/tags"),
					httpmock.JSONResponse([]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/tag-repo/rulesets"),
					httpmock.JSONResponse([]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/tag-repo"),
					httpmock.JSONResponse(map[string]interface{}{
						"security_and_analysis": map[string]interface{}{
							"secret_scanning":                 map[string]interface{}{"status": "enabled"},
							"secret_scanning_push_protection": map[string]interface{}{"status": "enabled"},
						},
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/octocat/tag-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "tag protection",
		},
		{
			name:  "code files trigger code scanning info",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "code-skill", heredoc.Doc(`
					---
					name: code-skill
					description: A skill with code
					license: MIT
					---
					Body.
				`))
				scriptDir := filepath.Join(dir, "skills", "code-skill", "scripts")
				require.NoError(t, os.MkdirAll(scriptDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(scriptDir, "helper.sh"), []byte("#!/bin/bash"), 0o644))
			},
			stubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/octocat/code-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{
						"names": []string{"agent-skills"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/code-repo/tags"),
					httpmock.JSONResponse([]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/code-repo/rulesets"),
					httpmock.JSONResponse([]map[string]interface{}{
						{"id": 1, "name": "tags", "target": "tag", "enforcement": "active"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/code-repo"),
					httpmock.JSONResponse(map[string]interface{}{
						"security_and_analysis": map[string]interface{}{
							"secret_scanning":                 map[string]interface{}{"status": "enabled"},
							"secret_scanning_push_protection": map[string]interface{}{"status": "enabled"},
						},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/code-repo/code-scanning/alerts"),
					httpmock.StatusStringResponse(404, "not found"),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/octocat/code-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					DryRun:    true,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStderr: "code scanning",
		},
		{
			name:  "manifest files trigger dependabot info",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "dep-skill", heredoc.Doc(`
					---
					name: dep-skill
					description: A skill with manifests
					license: MIT
					---
					Body.
				`))
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, "skills", "dep-skill", "package.json"),
					[]byte("{}"), 0o644,
				))
			},
			stubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/octocat/dep-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{
						"names": []string{"agent-skills"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/dep-repo/tags"),
					httpmock.JSONResponse([]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/dep-repo/rulesets"),
					httpmock.JSONResponse([]map[string]interface{}{
						{"id": 1, "name": "tags", "target": "tag", "enforcement": "active"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/dep-repo"),
					httpmock.JSONResponse(map[string]interface{}{
						"security_and_analysis": map[string]interface{}{
							"secret_scanning":                 map[string]interface{}{"status": "enabled"},
							"secret_scanning_push_protection": map[string]interface{}{"status": "enabled"},
						},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/octocat/dep-repo/vulnerability-alerts"),
					httpmock.StatusStringResponse(404, "not found"),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/octocat/dep-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					DryRun:    true,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStderr: "Dependabot",
		},
		{
			name:  "installed skill dirs not gitignored warns",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".agents", "skills", "installed"), 0o755))
				runGitInDir(t, dir, "init", "--initial-branch=main")
				runGitInDir(t, dir, "config", "user.email", "monalisa@github.com")
				runGitInDir(t, dir, "config", "user.name", "Monalisa Octocat")

				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					GitClient: &git.Client{RepoDir: dir},
				}
			},
			wantStdout: ".gitignore",
		},
		{
			name: "installed skill dirs gitignored no warning",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".agents", "skills", "installed"), 0o755))

				runGitInDir(t, dir, "init", "--initial-branch=main")
				runGitInDir(t, dir, "config", "user.email", "monalisa@github.com")
				runGitInDir(t, dir, "config", "user.name", "Monalisa Octocat")
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".agents/skills\n"), 0o644))
				runGitInDir(t, dir, "add", ".gitignore")
				runGitInDir(t, dir, "commit", "-m", "init")
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					GitClient: &git.Client{RepoDir: dir},
				}
			},
			wantStdout: "no git remote",
			verify: func(t *testing.T, dir string) {
				t.Helper()
				// The key assertion: .gitignored dirs should NOT produce a warning
			},
		},
		{
			name: "installed skill dirs git error warns about unverified status",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
				// Create install dir but do NOT init git so check-ignore will fail
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".agents", "skills", "installed"), 0o755))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					GitClient: &git.Client{RepoDir: dir},
				}
			},
			wantStdout: "may contain installed skills that are not gitignored",
		},
		{
			name: "no GitHub remote warns",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
				runGitInDir(t, dir, "init", "--initial-branch=main")
				runGitInDir(t, dir, "config", "user.email", "monalisa@github.com")
				runGitInDir(t, dir, "config", "user.name", "Monalisa Octocat")
				runGitInDir(t, dir, "remote", "add", "origin", "https://gitlab.com/hubot/bar.git")
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					GitClient: &git.Client{RepoDir: dir},
				}
			},
			wantStdout: "not a GitHub repository",
		},
		{
			name:  "fallback remote detection uses non-origin GitHub remote",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "octocat", "repo")
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin":   "https://gitlab.com/hubot/bar.git",
					"upstream": "git@github.com:octocat/repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					DryRun:    true,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStderr: "octocat/repo",
		},
		{
			name:  "publish adds missing topic via --tag",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				// topic missing
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{
						"names": []string{"golang"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/tags"),
					httpmock.JSONResponse([]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/rulesets"),
					httpmock.JSONResponse([]map[string]interface{}{
						{"id": 1, "name": "tags", "target": "tag", "enforcement": "active"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{
						"security_and_analysis": map[string]interface{}{
							"secret_scanning":                 map[string]interface{}{"status": "enabled"},
							"secret_scanning_push_protection": map[string]interface{}{"status": "enabled"},
						},
					}),
				)
				// addAgentSkillsTopic fetches topics again then PUTs
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{
						"names": []string{"golang"},
					}),
				)
				reg.Register(
					httpmock.REST("PUT", "repos/monalisa/skills-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{}),
				)
				// immutable releases
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.JSONResponse(map[string]interface{}{"enabled": true}),
				)
				// default branch
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{"default_branch": "main"}),
				)
				// create release
				reg.Register(
					httpmock.REST("POST", "repos/monalisa/skills-repo/releases"),
					httpmock.JSONResponse(map[string]interface{}{
						"html_url": "https://github.com/monalisa/skills-repo/releases/tag/v1.0.0",
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:  ios,
					Dir: dir,
					Tag: "v1.0.0",
					Prompter: &prompter.PrompterMock{
						ConfirmFunc: func(msg string, def bool) (bool, error) { return true, nil },
					},
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "Added \"agent-skills\" topic",
		},
		{
			name: "tag suggestion uses existing tags",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{
						"names": []string{"agent-skills"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/tags"),
					httpmock.JSONResponse([]map[string]interface{}{
						{"name": "v2.3.4"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/rulesets"),
					httpmock.JSONResponse([]map[string]interface{}{
						{"id": 1, "name": "tags", "target": "tag", "enforcement": "active"},
					}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{
						"security_and_analysis": map[string]interface{}{
							"secret_scanning":                 map[string]interface{}{"status": "enabled"},
							"secret_scanning_push_protection": map[string]interface{}{"status": "enabled"},
						},
					}),
				)
				// immutable releases
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.JSONResponse(map[string]interface{}{"enabled": true}),
				)
				// default branch
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{"default_branch": "main"}),
				)
				// create release with the suggested v2.3.5 tag
				reg.Register(
					httpmock.REST("POST", "repos/monalisa/skills-repo/releases"),
					httpmock.JSONResponse(map[string]interface{}{
						"html_url": "https://github.com/monalisa/skills-repo/releases/tag/v2.3.5",
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					Tag:       "v2.3.5",
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "Published v2.3.5",
		},
		{
			name: "duplicate tag errors",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					Tag:       "v1.0.0", // same as stubAllSecureRemote's existing tag
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantErr: "tag v1.0.0 already exists",
		},
		{
			name: "valid skill non-tty plain output",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "git-commit", heredoc.Doc(`
					---
					name: git-commit
					description: A skill for writing good git commits
					allowed-tools: git
					license: MIT
					---
					You are a git commit expert.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:        ios,
					Dir:       dir,
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "ok",
		},
		{
			name: "no remote and non-tty shows validation passed message",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			opts: func(ios *iostreams.IOStreams, dir string, _ *httpmock.Registry) *PublishOptions {
				t.Helper()
				return &PublishOptions{
					IO:  ios,
					Dir: dir,
				}
			},
			wantStdout: "ok",
		},
		{
			name:  "interactive publish with topic and semver tag",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				// No topic yet, first GET for diagnostic check
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{"names": []string{}}),
				)
				// Second GET inside addAgentSkillsTopic
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/topics"),
					httpmock.JSONResponse(map[string]interface{}{"names": []string{}}),
				)
				// Add topic
				reg.Register(
					httpmock.REST("PUT", "repos/monalisa/skills-repo/topics"),
					httpmock.StringResponse("{}"),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/tags"),
					httpmock.JSONResponse([]map[string]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/rulesets"),
					httpmock.JSONResponse([]map[string]interface{}{}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{
						"default_branch": "main",
						"security_and_analysis": map[string]interface{}{
							"secret_scanning":                 map[string]interface{}{"status": "enabled"},
							"secret_scanning_push_protection": map[string]interface{}{"status": "enabled"},
						},
					}),
				)
				// Immutable releases already enabled
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.JSONResponse(map[string]interface{}{"enabled": true}),
				)
				// Create release
				reg.Register(
					httpmock.REST("POST", "repos/monalisa/skills-repo/releases"),
					httpmock.JSONResponse(map[string]interface{}{
						"html_url": "https://github.com/monalisa/skills-repo/releases/tag/v1.0.0",
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				confirmCall := 0
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:  ios,
					Dir: dir,
					Prompter: &prompter.PrompterMock{
						ConfirmFunc: func(msg string, def bool) (bool, error) {
							confirmCall++
							return true, nil // accept topic + final confirm
						},
						SelectFunc: func(msg string, def string, opts []string) (int, error) {
							return 0, nil // semver strategy
						},
						InputFunc: func(msg string, def string) (string, error) {
							return "v1.0.0", nil // accept suggested tag
						},
					},
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "Published v1.0.0",
		},
		{
			name:  "interactive publish with custom tag",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.JSONResponse(map[string]interface{}{"enabled": true}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{"default_branch": "main"}),
				)
				reg.Register(
					httpmock.REST("POST", "repos/monalisa/skills-repo/releases"),
					httpmock.JSONResponse(map[string]interface{}{
						"html_url": "https://github.com/monalisa/skills-repo/releases/tag/beta-1",
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:  ios,
					Dir: dir,
					Prompter: &prompter.PrompterMock{
						ConfirmFunc: func(msg string, def bool) (bool, error) {
							return true, nil
						},
						SelectFunc: func(msg string, def string, opts []string) (int, error) {
							return 1, nil // custom tag strategy
						},
						InputFunc: func(msg string, def string) (string, error) {
							return "beta-1", nil
						},
					},
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "Published beta-1",
		},
		{
			name:  "interactive publish declined at final confirm",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.JSONResponse(map[string]interface{}{"enabled": true}),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{"default_branch": "main"}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				confirmCall := 0
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:  ios,
					Dir: dir,
					Prompter: &prompter.PrompterMock{
						ConfirmFunc: func(msg string, def bool) (bool, error) {
							confirmCall++
							if confirmCall >= 1 {
								return false, nil // decline final confirm
							}
							return true, nil
						},
						SelectFunc: func(msg string, def string, opts []string) (int, error) {
							return 0, nil
						},
						InputFunc: func(msg string, def string) (string, error) {
							return "v1.0.1", nil
						},
					},
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantErr:    "CancelError",
			wantStderr: "Publish cancelled",
		},
		{
			name:  "interactive immutable releases prompt",
			isTTY: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeSkill(t, dir, "my-skill", heredoc.Doc(`
					---
					name: my-skill
					description: A skill
					license: MIT
					---
					Body.
				`))
			},
			stubs: func(reg *httpmock.Registry) {
				stubAllSecureRemote(reg, "monalisa", "skills-repo")
				// Immutable releases NOT enabled
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.JSONResponse(map[string]interface{}{"enabled": false}),
				)
				// Enable immutable releases
				reg.Register(
					httpmock.REST("PATCH", "repos/monalisa/skills-repo/immutable-releases"),
					httpmock.StringResponse("{}"),
				)
				reg.Register(
					httpmock.REST("GET", "repos/monalisa/skills-repo"),
					httpmock.JSONResponse(map[string]interface{}{"default_branch": "main"}),
				)
				reg.Register(
					httpmock.REST("POST", "repos/monalisa/skills-repo/releases"),
					httpmock.JSONResponse(map[string]interface{}{
						"html_url": "https://github.com/monalisa/skills-repo/releases/tag/v1.0.1",
					}),
				)
			},
			opts: func(ios *iostreams.IOStreams, dir string, reg *httpmock.Registry) *PublishOptions {
				t.Helper()
				initGitRepo(t, dir, map[string]string{
					"origin": "https://github.com/monalisa/skills-repo.git",
				})
				return &PublishOptions{
					IO:  ios,
					Dir: dir,
					Prompter: &prompter.PrompterMock{
						ConfirmFunc: func(msg string, def bool) (bool, error) {
							return true, nil // accept all confirms (immutable + final)
						},
						SelectFunc: func(msg string, def string, opts []string) (int, error) {
							return 0, nil
						},
						InputFunc: func(msg string, def string) (string, error) {
							return "v1.0.1", nil
						},
					},
					GitClient: &git.Client{},
					client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
					host:      "github.com",
				}
			},
			wantStdout: "Enabled immutable releases",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)

			ios, _, stdout, stderr := iostreams.Test()
			ios.SetStdoutTTY(tt.isTTY)
			ios.SetStdinTTY(tt.isTTY)
			ios.SetStderrTTY(tt.isTTY)

			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			if tt.stubs != nil {
				tt.stubs(reg)
			}

			opts := tt.opts(ios, dir, reg)
			err := publishRun(opts)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tt.wantStdout != "" {
				assert.Contains(t, stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" {
				assert.Contains(t, stderr.String(), tt.wantStderr)
			}
			if tt.verify != nil {
				tt.verify(t, dir)
			}
		})
	}
}

func TestDetectGitHubRemote_UsesDir(t *testing.T) {
	// Create two separate git repos: "cwd-repo" simulates the working directory
	// and "target-repo" simulates the directory argument passed to publish.
	cwdRepo := t.TempDir()
	initGitRepo(t, cwdRepo, map[string]string{
		"origin": "https://github.com/monalisa/cwd-repo.git",
	})

	targetRepo := t.TempDir()
	initGitRepo(t, targetRepo, map[string]string{
		"origin": "https://github.com/monalisa/target-repo.git",
	})

	// gitClient points at cwd-repo (simulating factory-provided client)
	gitClient := &git.Client{RepoDir: cwdRepo}

	// detectGitHubRemote should use targetRepo's remotes, not cwdRepo's
	repo, err := detectGitHubRemote(gitClient, targetRepo)
	require.NoError(t, err)
	require.NotNil(t, repo)
	assert.Equal(t, "monalisa", repo.Repo.RepoOwner())
	assert.Equal(t, "target-repo", repo.Repo.RepoName())
}

func TestPublishRun_DirArgUsesTargetRemote(t *testing.T) {
	// Regression test: when a directory argument is provided, remote detection
	// must use that directory's git remotes, not the factory client's directory.
	//
	// Scenario:
	//   1. User is in cwd-repo (has remote → monalisa/cwd-repo)
	//   2. User runs: gh skill publish /path/to/target-repo
	//   3. target-repo has remote → monalisa/target-repo
	//   4. API calls must go to target-repo, NOT cwd-repo

	cwdRepo := t.TempDir()
	initGitRepo(t, cwdRepo, map[string]string{
		"origin": "https://github.com/monalisa/cwd-repo.git",
	})

	targetRepo := t.TempDir()
	initGitRepo(t, targetRepo, map[string]string{
		"origin": "https://github.com/monalisa/target-repo.git",
	})

	writeSkill(t, targetRepo, "my-skill", heredoc.Doc(`
		---
		name: my-skill
		description: A test skill
		license: MIT
		---
		Body text.
	`))

	ios, _, stdout, _ := iostreams.Test()
	ios.SetStdoutTTY(true)
	ios.SetStdinTTY(true)
	ios.SetStderrTTY(true)

	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Stub API calls for target-repo (the correct repo).
	// If the bug is present, these stubs won't be called because the code
	// would try to hit cwd-repo endpoints instead, and reg.Verify would fail.
	stubAllSecureRemote(reg, "monalisa", "target-repo")

	err := publishRun(&PublishOptions{
		IO:        ios,
		Dir:       targetRepo,
		DryRun:    true,
		GitClient: &git.Client{RepoDir: cwdRepo},
		client:    api.NewClientFromHTTP(&http.Client{Transport: reg}),
		host:      "github.com",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "1 skill(s) validated successfully")
}

// writeSkill creates skills/<name>/SKILL.md with the given content.
func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	skillDir := filepath.Join(dir, "skills", name)
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))
}

// runGitInDir runs a git command in the given directory with isolation env vars.
func runGitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// newTestGitClientWithUpstream creates a git repo with a local bare "remote"
// and an initial commit, so we can test push/rev-list behavior realistically.
// It returns the git client and the working directory path.
func newTestGitClientWithUpstream(t *testing.T) (*git.Client, string) {
	t.Helper()
	parentDir := t.TempDir()
	bareDir := filepath.Join(parentDir, "upstream.git")
	workDir := filepath.Join(parentDir, "work")

	gitEnv := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+parentDir)

	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = gitEnv
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	// Create bare upstream
	require.NoError(t, os.MkdirAll(bareDir, 0o755))
	run(bareDir, "init", "--bare", "--initial-branch=main")

	// Clone into working dir
	c := exec.Command("git", "clone", bareDir, workDir)
	c.Env = gitEnv
	out, err := c.CombinedOutput()
	require.NoError(t, err, "git clone: %s", out)

	run(workDir, "config", "user.email", "monalisa@github.com")
	run(workDir, "config", "user.name", "Monalisa Octocat")

	// Create initial commit and push
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Test"), 0o644))
	run(workDir, "add", ".")
	run(workDir, "commit", "-m", "initial commit")
	run(workDir, "push", "origin", "main")

	return &git.Client{
		RepoDir: workDir,
		GitPath: "git",
		Stderr:  &bytes.Buffer{},
		Stdin:   &bytes.Buffer{},
		Stdout:  &bytes.Buffer{},
	}, workDir
}

func TestEnsurePushed(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, workDir string)
		verify     func(t *testing.T, workDir string)
		wantErr    string
		wantStderr string
	}{
		{
			name: "no unpushed commits is a no-op",
			setup: func(_ *testing.T, _ string) {
				// initial commit already pushed by helper
			},
		},
		{
			name: "unpushed commits are pushed automatically",
			setup: func(t *testing.T, workDir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(workDir, "new.txt"), []byte("new"), 0o644))
				runGitInDir(t, workDir, "add", ".")
				runGitInDir(t, workDir, "commit", "-m", "unpushed change")
			},
			verify: func(t *testing.T, workDir string) {
				t.Helper()
				// After push, rev-list should show 0 unpushed commits
				cmd := exec.Command("git", "-C", workDir, "rev-list", "--count", "@{push}..HEAD")
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+workDir)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "rev-list: %s", out)
				assert.Equal(t, "0", strings.TrimSpace(string(out)))
			},
			wantStderr: "Pushing main to origin",
		},
		{
			name: "new branch never pushed is pushed automatically",
			setup: func(t *testing.T, workDir string) {
				t.Helper()
				runGitInDir(t, workDir, "checkout", "-b", "feature")
				require.NoError(t, os.WriteFile(filepath.Join(workDir, "feat.txt"), []byte("feat"), 0o644))
				runGitInDir(t, workDir, "add", ".")
				runGitInDir(t, workDir, "commit", "-m", "new branch commit")
			},
			verify: func(t *testing.T, workDir string) {
				t.Helper()
				// After push, the branch should exist on the remote
				cmd := exec.Command("git", "-C", workDir, "rev-list", "--count", "@{push}..HEAD")
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+workDir)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "rev-list: %s", out)
				assert.Equal(t, "0", strings.TrimSpace(string(out)))
			},
			wantStderr: "Pushing feature to origin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gitClient, workDir := newTestGitClientWithUpstream(t)
			tt.setup(t, workDir)

			ios, _, _, stderr := iostreams.Test()
			ios.SetStdoutTTY(true)
			ios.SetStderrTTY(true)

			opts := &PublishOptions{
				IO:        ios,
				GitClient: gitClient,
			}

			err := ensurePushed(opts, workDir, "origin")

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tt.wantStderr != "" {
				assert.Contains(t, stderr.String(), tt.wantStderr)
			}
			if tt.verify != nil {
				tt.verify(t, workDir)
			}
		})
	}
}
