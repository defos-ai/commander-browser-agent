//go:build hermesintegration

package fixtures

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

//go:embed hermes_workflow.py
var hermesWorkflowSite string

// HermesWorkflowSiteScript starts only the loopback HTTP fixture inside an
// existing worker. Synthetic site values travel over its child's private stdin;
// they are never model settings, environment values or server command arguments.
func HermesWorkflowSiteScript(username, password, code, keyEcho string) string {
	configuration, err := json.Marshal(map[string]string{
		"username": username, "password": password, "code": code, "key_echo": keyEcho,
	})
	if err != nil {
		panic("invalid synthetic Hermes workflow configuration")
	}
	defer clear(configuration)
	return fmt.Sprintf(`import base64, json, select, subprocess, sys
source = base64.b64decode(%q).decode()
configuration = base64.b64decode(%q)
server = subprocess.Popen([sys.executable, '-I', '-S', '-c', source],
    stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    cwd='/home/commander', start_new_session=True)
server.stdin.write(configuration + b'\n')
server.stdin.close()
if not select.select([server.stdout], [], [], 5)[0]:
    server.kill()
    server.wait(timeout=2)
    raise RuntimeError('workflow_fixture_unavailable')
ready = server.stdout.readline(1024)
try:
    address = json.loads(ready)
    assert set(address) == {'url'} and address['url'].startswith('http://127.0.0.1:')
except Exception:
    server.kill()
    server.wait(timeout=2)
    raise RuntimeError('workflow_fixture_unavailable') from None
print(json.dumps(address))
`, base64.StdEncoding.EncodeToString([]byte(hermesWorkflowSite)), base64.StdEncoding.EncodeToString(configuration))
}

// HermesWorkflowStatsScript returns only non-secret website submission counts.
func HermesWorkflowStatsScript(address string) string {
	encoded, err := json.Marshal(address)
	if err != nil {
		panic("invalid synthetic Hermes workflow address")
	}
	return fmt.Sprintf(`import base64, json, urllib.parse, urllib.request
address = json.loads(base64.b64decode(%q))
parts = urllib.parse.urlsplit(address)
assert parts.scheme == 'http' and parts.hostname == '127.0.0.1' and parts.path in ('', '/')
with urllib.request.urlopen(address.rstrip('/') + '/stats', timeout=2) as response:
    counts = json.loads(response.read(1024))
print(json.dumps({name: str(counts[name]) for name in ('password_posts', 'code_posts')}))
`, base64.StdEncoding.EncodeToString(encoded))
}
