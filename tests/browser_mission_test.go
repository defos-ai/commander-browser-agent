//go:build integration

package browser_test

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/missions"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/host"
	"github.com/defos-ai/commander/framework/id"
	"github.com/defos-ai/commander/framework/testing/missiontest"
)

//go:embed fixtures/browser-mission.json
var browserMission []byte

type accountReport struct {
	Text         string `json:"report_text"`
	ScreenshotID string `json:"screenshot_artifact_id"`
}

func TestBrowserMissionThroughAPI(t *testing.T) {
	config := missiontest.Config{
		URL:         os.Getenv("COMMANDER_API_URL"),
		Credentials: host.BasicCredentials{Username: os.Getenv("API_USERNAME"), Password: os.Getenv("API_PASSWORD")},
	}
	driver := missiontest.Connect(t, config)
	control := fixtures.NewCommander(t, config.URL, config.Credentials)
	site := fixtures.NewMission(t)

	definition, err := missions.LoadMission(browserMission)
	if err != nil {
		t.Fatal(err)
	}
	definition.Name += "-" + id.New()
	driver.ImportMission(definition)
	driver.WaitForActions(definition.RequiredActions...)

	// The fixture binds an explicit loopback port, so its URL is also its canonical origin.
	const profile = "test-account"
	credentialBase := fmt.Sprintf("browser-login-%s-%x", profile, sha256.Sum256([]byte(site.URL+"\x00"+profile)))
	control.SetSecret(credentialBase+"-username", fixtures.Username)
	control.SetSecret(credentialBase+"-password", fixtures.Password)
	control.SetSecret(fmt.Sprintf("browser-model-api-key-%x", sha256.Sum256([]byte(site.Endpoint))), fixtures.ModelKey)

	accountURL := site.URL + "/account"
	op := driver.Start(definition.Name, map[string]string{"account_url": accountURL})
	login := driver.Assign(op.ID, missiontest.Assignment{ActionName: actions.LoginName, Input: actions.LoginInput{
		URL: site.URL + "/login", VerificationURL: accountURL, ExpectedText: fixtures.AccountText,
	}})
	login = answerQuestion(t, driver, login, "browser_login_profile", profile)
	control.DeliverOTP(login, credentialBase+"-otp", fixtures.OTP)
	login = successfulTask(t, driver, login, sessions.Open)
	signedIn := decode[actions.LoginOutput](t, login.Result.Output)
	if signedIn.Engine != "lightpanda" || signedIn.Status != "verified" || !signedIn.Submitted || signedIn.Reason != "verification_matched" {
		t.Fatalf("login returned status %q and reason %q", signedIn.Status, signedIn.Reason)
	}

	browse := continueWith(t, driver, login, actions.BrowseName, actions.BrowseInput{
		Objective: "Read the report on the signed-in account page and return its report text.",
		URL:       accountURL, MaxIterations: 5, MaxTokensPerResponse: 512,
	})
	answerQuestion(t, driver, browse, "browser_model_provider_1", "Other (OpenAI-compatible)")
	answerQuestion(t, driver, browse, "browser_model_1", fixtures.ModelName)
	answerQuestion(t, driver, browse, "browser_model_url_1", site.Endpoint)
	browse = successfulTask(t, driver, browse, sessions.Open)
	if browse.Result.Session.Reference != login.Result.Session.Reference {
		t.Fatal("browse did not inherit the login session")
	}
	observation := decode[actions.BrowseOutput](t, browse.Result.Output)
	if observation.Runtime != "hermes" || observation.Engine != "lightpanda" || observation.Status != "completed" ||
		observation.Text != site.ReportText || observation.ModelRequests < 2 {
		t.Fatalf("browse failed to read the authenticated report (status %s, requests %d)", observation.Status, observation.ModelRequests)
	}
	browseFiles := downloadFiles(t, driver, browse, map[string]string{"browse.json": "application/json"})
	if decode[actions.BrowseOutput](t, browseFiles["browse.json"]) != observation {
		t.Fatal("downloaded browse.json differs from the accepted browse result")
	}

	capture := continueWith(t, driver, browse, actions.ScreenshotName, actions.ScreenshotInput{Viewport: "mobile"})
	capture = successfulTask(t, driver, capture, sessions.Use)
	if capture.Result.Session.Reference != login.Result.Session.Reference {
		t.Fatal("screenshot did not inherit the authenticated browser session")
	}
	want := actions.ScreenshotOutput{Engine: "lightpanda", Renderer: "lightpanda-text-snapshot", Viewport: "mobile",
		Width: 390, Height: 844, MediaType: "image/png"}
	files := downloadFiles(t, driver, capture, map[string]string{"screenshot.json": "application/json", "screenshot.png": "image/png"})
	if decode[actions.ScreenshotOutput](t, capture.Result.Output) != want || decode[actions.ScreenshotOutput](t, files["screenshot.json"]) != want {
		t.Fatal("screenshot result or downloaded metadata differs from the requested text viewport")
	}
	frame, err := png.Decode(bytes.NewReader(files["screenshot.png"]))
	if err != nil || frame.Bounds().Dx() != want.Width || frame.Bounds().Dy() != want.Height {
		t.Fatal("downloaded screenshot is not a complete PNG of the requested dimensions")
	}
	if uniformImage(frame) {
		t.Fatal("the report screenshot contains only a single color")
	}
	site.AssertUsed(t)
	driver.Resolve(op.ID, driver.WaitForContinuation(op.ID, capture.ID))
	control.CloseSession(capture)

	result := accountReport{Text: observation.Text, ScreenshotID: capture.Result.Artifacts[0].ID}
	completed := driver.Complete(op.ID, result)
	report := driver.Report(op.ID)
	if completed.Status != operations.Completed || len(completed.Tasks) != 3 || completed.ReportID != report.ID ||
		decode[accountReport](t, completed.Output) != result || decode[accountReport](t, report.Output) != result {
		t.Fatal("completed mission did not retain the observed report text and screenshot reference")
	}
}

func uniformImage(frame image.Image) bool {
	bounds := frame.Bounds()
	background := frame.At(bounds.Min.X, bounds.Min.Y)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if frame.At(x, y) != background {
				return false
			}
		}
	}
	return true
}

// Answer the exact saved question and return the running task that requested it.
func answerQuestion(t *testing.T, driver *missiontest.Driver, task operations.Task, key, answer string) operations.Task {
	t.Helper()
	var questionID string
	var running operations.Task
	driver.WaitFor(task.OperationID, func(op operations.Operation) bool {
		for _, current := range op.Tasks {
			if current.ID == task.ID {
				running = current
				if current.Status != operations.TaskQueued && current.Status != operations.TaskRunning {
					t.Fatalf("%s finished with status %s before asking %s", task.ActionName, current.Status, key)
				}
			}
		}
		for _, q := range op.Questions {
			if q.TaskID == task.ID && q.Pending() {
				if q.Key != key || q.CommandID != running.CommandID {
					t.Fatalf("%s asked an unexpected execution question while waiting for %s", task.ActionName, key)
				}
				questionID = q.ID
				return true
			}
		}
		return false
	})
	driver.Answer(task.OperationID, questionID, answer)
	return running
}

func successfulTask(t *testing.T, driver *missiontest.Driver, task operations.Task, mode sessions.Mode) operations.Task {
	t.Helper()
	task = driver.WaitForTask(task.OperationID, task.ID)
	if task.Status != operations.TaskSucceeded || task.Result == nil || task.Result.Session == nil {
		t.Fatalf("%s task %s finished with status %s and no successful session result", task.ActionName, task.ID, task.Status)
	}
	resource := task.Result.Session
	if resource.State != "connected" || task.SessionIntent == nil || task.SessionIntent.Mode != mode ||
		task.SessionIntent.ID != resource.ID || task.SessionIntent.ScopeID != resource.ScopeID || task.AgentID != resource.OwnerAgentID {
		t.Fatalf("%s result did not identify its assigned connected session", task.ActionName)
	}
	return task
}

func continueWith(t *testing.T, driver *missiontest.Driver, parent operations.Task, name string, input any) operations.Task {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	review := driver.WaitForContinuation(parent.OperationID, parent.ID)
	op := driver.Resolve(parent.OperationID, review, actionmodel.NextAction{ActionName: name, Input: raw})
	for _, child := range op.Tasks {
		if child.ParentTaskID == parent.ID && child.ActionName == name {
			return child
		}
	}
	t.Fatalf("resolving %s did not create a %s child", parent.ActionName, name)
	return operations.Task{}
}

func downloadFiles(t *testing.T, driver *missiontest.Driver, task operations.Task, want map[string]string) map[string][]byte {
	t.Helper()
	if len(task.Result.Artifacts) != 1 || len(task.Result.Artifacts[0].Files) != len(want) {
		t.Fatalf("%s did not return the expected artifact files", task.ActionName)
	}
	artifact := task.Result.Artifacts[0]
	files := make(map[string][]byte, len(want))
	for _, file := range artifact.Files {
		mediaType, exists := want[file.Name]
		if !exists || mediaType != file.MediaType || file.Size == 0 || files[file.Name] != nil {
			t.Fatalf("%s returned unexpected or empty artifact file metadata", task.ActionName)
		}
		files[file.Name] = driver.Download(artifact.ID, file)
	}
	return files
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if !json.Valid(data) || decoder.Decode(&value) != nil {
		t.Fatalf("invalid %T output", value)
	}
	return value
}
