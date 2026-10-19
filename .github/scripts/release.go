// Release builds metadata and the release manifest from the tested agent source.
package main

import (
	"bytes"
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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
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
	tmpl, err := template.New(filepath.Base(source)).Option("missingkey=error").ParseFiles(source)
	if err != nil {
		return err
	}
	var result bytes.Buffer
	if err := tmpl.Execute(&result, struct {
		metadata
		Native      []nativeAsset
		DockerImage string
	}{data, native, data.Image + "@" + digest}); err != nil {
		return err
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
