// Release builds metadata and the release manifest from the tested agent source.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

type metadata struct {
	Version string `json:"version"`
	Type    string `json:"type"`
	Binary  string `json:"binary"`
	Image   string `json:"image"`
}

type nativeAsset struct {
	Platform string
	Asset    string
	SHA256   string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "metadata" && args[0] != "manifest") {
		return errors.New("usage: release metadata|manifest --version vX.Y.Z --repository owner/repo")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	version := flags.String("version", "", "release version")
	repository := flags.String("repository", "", "GitHub owner/repository")
	var digest, assets, source string
	if args[0] == "manifest" {
		flags.StringVar(&digest, "digest", "", "published image digest")
		flags.StringVar(&assets, "assets", "dist", "release asset directory")
		flags.StringVar(&source, "template", "agent.yaml.tmpl", "manifest template")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	data, err := readMetadata(*version, *repository)
	if err != nil {
		return err
	}
	if args[0] == "metadata" {
		return json.NewEncoder(os.Stdout).Encode(data)
	}
	return writeManifest(data, digest, assets, source)
}

func readMetadata(version, repository string) (metadata, error) {
	if !regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(version) || version == "v0.0.0" {
		return metadata{}, errors.New("version must be a nonzero stable vMAJOR.MINOR.PATCH tag")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(repository) {
		return metadata{}, errors.New("repository must be a GitHub owner/repository")
	}
	agentType, err := readAgentType()
	if err != nil {
		return metadata{}, err
	}
	binary := "commander-" + agentType
	return metadata{
		Version: version,
		Type:    agentType,
		Binary:  binary,
		Image:   "ghcr.io/" + strings.ToLower(strings.Split(repository, "/")[0]) + "/" + binary,
	}, nil
}

func readAgentType() (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "agent.go", nil, 0)
	if err != nil {
		return "", err
	}
	var agentType string
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			continue
		}
		for _, spec := range decl.Specs {
			value := spec.(*ast.ValueSpec)
			for i, name := range value.Names {
				if name.Name != "AgentType" {
					continue
				}
				if agentType != "" || i >= len(value.Values) {
					return "", errors.New("AgentType must be one directly declared string literal")
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return "", errors.New("AgentType must be a string literal")
				}
				agentType, err = strconv.Unquote(literal.Value)
				if err != nil {
					return "", err
				}
			}
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`).MatchString(agentType) {
		return "", errors.New("AgentType must be a lowercase Docker-compatible name in agent.go")
	}
	return agentType, nil
}

func writeManifest(data metadata, digest, assets, source string) error {
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		return errors.New("digest must be sha256: followed by 64 lowercase hexadecimal characters")
	}
	var native []nativeAsset
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			asset := data.Binary + "_" + platform + "_" + arch
			if platform == "windows" {
				asset += ".exe"
			}
			checksum, err := hashAsset(filepath.Join(assets, asset))
			if err != nil {
				return err
			}
			native = append(native, nativeAsset{Platform: platform + "/" + arch, Asset: asset, SHA256: checksum})
		}
	}
	description, err := readDescription(data.Type)
	if err != nil {
		return err
	}
	tmpl, err := template.New(filepath.Base(source)).Option("missingkey=error").ParseFiles(source)
	if err != nil {
		return err
	}
	var result bytes.Buffer
	if err := tmpl.Execute(&result, struct {
		metadata
		Native           []nativeAsset
		DockerImage      string
		Responsibilities []string
		Actions          []actionDescription
	}{data, native, data.Image + "@" + digest, description.Responsibilities, description.Actions}); err != nil {
		return err
	}
	if result.Len() > 64<<10 {
		return errors.New("rendered agent.yaml exceeds 64 KiB")
	}
	// Complete validation and rendering before replacing a previously generated manifest.
	file, err := os.CreateTemp(assets, ".agent-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(result.Bytes()); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(file.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(assets, "agent.yaml"))
}

func hashAsset(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("asset %s must be a nonempty regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

type actionDescription struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type agentDescription struct {
	Type             string              `json:"type"`
	Responsibilities []string            `json:"responsibilities"`
	Actions          []actionDescription `json:"actions"`
}

// The import follows go.mod, so copying the template never leaves a hardcoded
// example-agent import in the release tooling. Only pure metadata is called.
const descriptionProgram = `package main
import (
    "encoding/json"
    "os"
    described %s
)
func main() {
    descriptions := described.ActionDescriptions()
    actions := make([]map[string]string, 0, len(descriptions))
    for _, value := range descriptions {
        actions = append(actions, map[string]string{"name": value.Name, "description": value.Description})
    }
    if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
        "type": string(described.AgentType),
        "responsibilities": described.Responsibilities(),
        "actions": actions,
    }); err != nil {
        os.Exit(1)
    }
}
`

func readDescription(agentType string) (agentDescription, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	moduleJSON, err := runGo(ctx, "mod", "edit", "-json")
	if err != nil {
		return agentDescription{}, err
	}
	var module struct{ Module *struct{ Path string } }
	if json.Unmarshal(moduleJSON, &module) != nil || module.Module == nil || strings.TrimSpace(module.Module.Path) == "" {
		return agentDescription{}, errors.New("cannot read the agent module path from go.mod")
	}
	directory, err := os.MkdirTemp("", "agent-description-")
	if err != nil {
		return agentDescription{}, errors.New("cannot prepare agent description exporter")
	}
	defer os.RemoveAll(directory)
	program := filepath.Join(directory, "main.go")
	if err := os.WriteFile(program, []byte(fmt.Sprintf(descriptionProgram, strconv.Quote(module.Module.Path))), 0o600); err != nil {
		return agentDescription{}, errors.New("cannot write agent description exporter")
	}
	output, err := runGo(ctx, "run", program)
	if err != nil {
		return agentDescription{}, err
	}
	var result agentDescription
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Type != agentType {
		return agentDescription{}, errors.New("agent description must match the declared AgentType")
	}
	if len(result.Responsibilities) == 0 || len(result.Responsibilities) > 100 || len(result.Actions) == 0 || len(result.Actions) > 100 {
		return agentDescription{}, errors.New("agent description requires 1 to 100 responsibilities and actions")
	}
	validText := func(value string) bool {
		return value != "" && strings.TrimSpace(value) == value && len(value) <= 8<<10 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
	}
	for _, value := range result.Responsibilities {
		if !validText(value) {
			return agentDescription{}, errors.New("agent responsibilities must be trimmed nonblank text of at most 8 KiB")
		}
	}
	namePattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	seen := make(map[string]bool, len(result.Actions))
	for _, value := range result.Actions {
		if !namePattern.MatchString(value.Name) || len(value.Name) > 128 || seen[value.Name] || !validText(value.Description) {
			return agentDescription{}, errors.New("agent actions require unique valid names and trimmed nonblank descriptions of at most 8 KiB")
		}
		seen[value.Name] = true
	}
	sort.Slice(result.Actions, func(i, j int) bool { return result.Actions[i].Name < result.Actions[j].Name })
	return result, nil
}

type boundedGoOutput struct{ bytes.Buffer }

func (output *boundedGoOutput) Write(data []byte) (int, error) {
	if len(data) > (64<<10)-output.Len() {
		return 0, errors.New("agent description output exceeds 64 KiB")
	}
	return output.Buffer.Write(data)
}

func runGo(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "go", arguments...)
	command.WaitDelay = time.Second
	// Release jobs may have GH_TOKEN in their environment. Exporters never
	// inherit it, model keys or other deployment configuration.
	for _, name := range []string{"PATH", "HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOSUMDB", "CGO_ENABLED"} {
		if value, exists := os.LookupEnv(name); exists {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	command.Env = append(command.Env, "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off")
	var output boundedGoOutput
	command.Stdout, command.Stderr = &output, io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot export the agent's static description; preserve Responsibilities and ActionDescriptions and verify its Go build")
	}
	return output.Bytes(), nil
}
