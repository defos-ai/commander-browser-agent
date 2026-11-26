package release_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReleaseNotes(t *testing.T) {
	const title = "browser-agent v1.2.3"
	const heading = "## " + title + "\n"
	const body = "Updated the browser.\n\n### Upgrade\n\nUse the new version.\n"
	for _, test := range []struct {
		name, notes, want string
	}{
		{"selected section", "# Release v1.2.3\n\nPending release.\n\n" + heading + " \t\n" + body + "\t\n## Other agent\nOther notes.\n", body},
		{"empty section", heading + "\n \t\n## Other agent\nOther notes.\n", ""},
		{"duplicate section", heading + "First body.\n" + heading + "Second body.\n", ""},
		{"missing exact heading", "## " + title + "0\nOther version.\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release notes.md")
			writeFile(t, path, test.notes)
			script := filepath.Join("..", "scripts", "release-notes.sh")
			out, err := exec.CommandContext(t.Context(), "bash", script, path, title).Output()
			if test.want == "" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("expected nonzero exit, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if string(out) != test.want {
				t.Errorf("stdout = %q, want %q", out, test.want)
			}
		})
	}
}
