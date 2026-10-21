package browser_test

import (
	"reflect"
	"testing"

	described "github.com/defos-ai/commander-browser-agent"
	"github.com/defos-ai/commander/framework/agent"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
)

func TestStaticDescriptionsMatchRuntimeCatalog(t *testing.T) {
	store, err := fsartifacts.NewArtifactFileFsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := described.NewAgent(agent.AgentConfig{ArtifactFileStore: store})
	if err != nil {
		t.Fatal(err)
	}
	runtime := instance.Describe()
	responsibilities := described.Responsibilities()
	if len(responsibilities) == 0 || !reflect.DeepEqual(responsibilities, runtime.Responsibilities) {
		t.Fatal("static responsibilities differ from registration")
	}
	descriptions := described.ActionDescriptions()
	if len(descriptions) == 0 || len(descriptions) != len(runtime.Actions) {
		t.Fatal("static action descriptions differ from the registered catalog")
	}
	for _, registered := range runtime.Actions {
		matched := false
		for _, value := range descriptions {
			if value.Name == registered.Name && value.Description == registered.Description && value.Description != "" {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("registered action %q is missing its static description", registered.Name)
		}
	}
	responsibilities[0] = "changed"
	descriptions[0].Description = "changed"
	if described.Responsibilities()[0] == "changed" || described.ActionDescriptions()[0].Description == "changed" {
		t.Fatal("description calls share mutable slices")
	}
}
