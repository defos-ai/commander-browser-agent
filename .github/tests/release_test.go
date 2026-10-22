package release_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/defos-ai/commander/framework/agent/manifest"
)

var releaseTool string

func TestMain(m *testing.M) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(source), "..", "..")
	directory, err := os.MkdirTemp("", "example-agent-release-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	releaseTool = filepath.Join(directory, "release")
	if runtime.GOOS == "windows" {
		releaseTool += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	command := exec.CommandContext(ctx, "go", "build", "-o", releaseTool, "./.github/scripts/release.go")
	command.Dir = root
	output, err := command.CombinedOutput()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build release tool: %v\n%s", err, output)
		os.RemoveAll(directory)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(directory)
	os.Exit(code)
}

func TestReleaseMetadata(t *testing.T) {
	directory := fixture(t)
	output, err := invoke(t, directory, "metadata", "--version", "v1.2.3", "--repository", "ExampleOrg/custom-repository")
	if err != nil {
		t.Fatalf("metadata: %v\n%s", err, output)
	}
	var metadata map[string]string
	if err := json.Unmarshal(output, &metadata); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"version": "v1.2.3", "type": "custom-agent", "binary": "commander-custom-agent",
		"image": "ghcr.io/exampleorg/commander-custom-agent",
	}
	if len(metadata) != len(expected) {
		t.Fatalf("unexpected metadata: %s", output)
	}
	for key, value := range expected {
		if metadata[key] != value {
			t.Errorf("%s = %q; want %q", key, metadata[key], value)
		}
	}
}

func TestReleaseMetadataRejectsInvalidInputs(t *testing.T) {
	for _, version := range []string{"", "v0.0.0", "0.1.0", "v01.0.0", "v1.01.0", "v1.0.01", "v1.0.0-rc.1", "v1.0.0+build", "v1.0.0\n"} {
		t.Run("version/"+version, func(t *testing.T) {
			assertFailure(t, fixture(t), "metadata", "--version", version, "--repository", "owner/repo")
		})
	}
	for _, repository := range []string{"", "owner", "https://github.com/owner/repo", "owner/repo/extra", "owner/repo@v1.0.0"} {
		t.Run("repository/"+repository, func(t *testing.T) {
			assertFailure(t, fixture(t), "metadata", "--version", "v0.1.0", "--repository", repository)
		})
	}
	for _, declaration := range []string{
		`const Other = "custom-agent"`,
		`var AgentType = "custom-agent"`,
		`const AgentType = "custom-" + "agent"`,
		`const AgentType = "Custom-Agent"`,
		`const AgentType = "bad/name"`,
		`const AgentType = ""`,
	} {
		t.Run(declaration, func(t *testing.T) {
			directory := fixture(t)
			writeFile(t, filepath.Join(directory, "agent.go"), "package example\n"+declaration+"\n")
			assertFailure(t, directory, "metadata", "--version", "v0.1.0", "--repository", "owner/repo")
		})
	}
}

func TestReleaseManifest(t *testing.T) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "SECRETS_ENCRYPTION_KEY"} {
		t.Setenv(name, "not-for-agent-description")
	}
	directory := fixture(t)
	output, err := invoke(t, directory, manifestArgs()...)
	if err != nil {
		t.Fatalf("manifest: %v\n%s", err, output)
	}
	contents, err := os.ReadFile(filepath.Join(directory, "dist", "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := manifest.Parse(contents, "v0.1.0")
	if err != nil {
		t.Fatalf("manifest is incompatible with Commander: %v", err)
	}
	if value.Type != "custom-agent" || !reflect.DeepEqual(value.Responsibilities, []string{"Analyze custom text"}) ||
		!reflect.DeepEqual(value.Actions, []manifest.Action{{Name: "analyze-custom", Description: "Count words in custom text."}}) {
		t.Fatalf("manifest omitted static metadata: %+v", value)
	}
	if len(value.Native) != 6 {
		t.Fatalf("manifest has %d native platforms; want 6", len(value.Native))
	}
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			asset := assetName(platform, arch)
			data, err := os.ReadFile(filepath.Join(directory, "dist", asset))
			if err != nil {
				t.Fatal(err)
			}
			want := manifest.NativeAsset{Asset: asset, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
			if got := value.Native[platform+"/"+arch]; got != want {
				t.Errorf("%s/%s asset = %+v; want %+v", platform, arch, got, want)
			}
		}
	}
	if value.Docker == nil || value.Docker.Image != "ghcr.io/exampleorg/commander-custom-agent@sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("manifest lost the published image digest: %+v", value.Docker)
	}
}

func TestReleaseManifestFailurePreservesExistingOutput(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"missing asset": func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, "dist", assetName("linux", "amd64"))); err != nil {
				t.Fatal(err)
			}
		},
		"empty asset": func(t *testing.T, directory string) {
			writeFile(t, filepath.Join(directory, "dist", assetName("linux", "amd64")), "")
		},
		"symlink asset": func(t *testing.T, directory string) {
			path := filepath.Join(directory, "dist", assetName("linux", "amd64"))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(directory, "dist", assetName("linux", "arm64")), path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		},
		"blank description": func(t *testing.T, directory string) {
			writeFile(t, filepath.Join(directory, "description.go"), strings.Replace(descriptionFixtureSource, "Count words in custom text.", " ", 1))
		},
		"duplicate action": func(t *testing.T, directory string) {
			original := `{Name: "analyze-custom", Description: "Count words in custom text."}`
			writeFile(t, filepath.Join(directory, "description.go"), strings.Replace(descriptionFixtureSource, original, original+", "+original, 1))
		},
		"empty responsibilities": func(t *testing.T, directory string) {
			writeFile(t, filepath.Join(directory, "description.go"), strings.Replace(descriptionFixtureSource, `[]string{"Analyze custom text"}`, `[]string{}`, 1))
		},
		"oversized description output": func(t *testing.T, directory string) {
			writeFile(t, filepath.Join(directory, "description.go"), strings.Replace(descriptionFixtureSource, "Analyze custom text", strings.Repeat("x", 64<<10), 1))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			directory := fixture(t)
			path := filepath.Join(directory, "dist", "agent.yaml")
			writeFile(t, path, "previous manifest\n")
			mutate(t, directory)
			assertFailure(t, directory, manifestArgs()...)
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != "previous manifest\n" {
				t.Fatalf("failed generation changed previous output: %q, %v", contents, err)
			}
		})
	}
}

func TestReleaseManifestRejectsDigest(t *testing.T) {
	for _, digest := range []string{"", "latest", strings.Repeat("a", 64), "sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 63), "sha256:" + strings.Repeat("z", 64)} {
		t.Run(digest, func(t *testing.T) {
			directory := fixture(t)
			args := manifestArgs()
			args = append(args, "--digest", digest)
			assertFailure(t, directory, args...)
			if _, err := os.Stat(filepath.Join(directory, "dist", "agent.yaml")); !os.IsNotExist(err) {
				t.Fatalf("invalid digest created manifest: %v", err)
			}
		})
	}
}

func fixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	writeFile(t, filepath.Join(directory, "agent.go"), "package example\nconst AgentType = \"custom-agent\"\n")
	writeFile(t, filepath.Join(directory, "go.mod"), "module example.invalid/custom-agent\n\ngo 1.26.7\n")
	writeFile(t, filepath.Join(directory, "description.go"), descriptionFixtureSource)
	if err := os.Mkdir(filepath.Join(directory, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			contents := "abc"
			if platform == "windows" {
				contents = "hello"
			}
			writeFile(t, filepath.Join(directory, "dist", assetName(platform, arch)), contents)
		}
	}
	return directory
}

func assetName(platform, arch string) string {
	name := "commander-custom-agent_" + platform + "_" + arch
	if platform == "windows" {
		name += ".exe"
	}
	return name
}

func manifestArgs() []string {
	return []string{"manifest", "--version", "v0.1.0", "--repository", "ExampleOrg/repo", "--digest", "sha256:" + strings.Repeat("a", 64), "--assets", "dist"}
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func invoke(t *testing.T, directory string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, releaseTool, args...)
	command.Dir = directory
	return command.CombinedOutput()
}

func assertFailure(t *testing.T, directory string, args ...string) {
	t.Helper()
	if output, err := invoke(t, directory, args...); err == nil {
		t.Fatalf("expected failure for %v, got %s", args, output)
	}
}

const descriptionFixtureSource = `package example

import "os"

type descriptionAction struct { Name, Description string }

func Responsibilities() []string {
    for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "SECRETS_ENCRYPTION_KEY"} {
        if os.Getenv(name) != "" { panic("release exporter inherited a secret") }
    }
    return []string{"Analyze custom text"}
}

func ActionDescriptions() []descriptionAction {
    return []descriptionAction{{Name: "analyze-custom", Description: "Count words in custom text."}}
}
`
