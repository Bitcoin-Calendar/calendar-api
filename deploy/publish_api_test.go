package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the printed recipe locally with only SSH, ownership and service/HTTP
// operations replaced. A hard link models the inode held by the running process:
// copying over the live path would corrupt that inode, even on macOS where
// ETXTBSY is not guaranteed.
func TestManualRollbackRecipe(t *testing.T) {
	script, err := os.ReadFile("publish-api.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "manual_commands() {")
	if start < 0 {
		t.Fatal("manual_commands function missing")
	}
	end := strings.Index(string(script[start:]), "\n}")
	if end < 0 {
		t.Fatal("manual_commands function unterminated")
	}
	function := string(script[start : start+end+2])
	for _, tc := range []struct {
		name, directory string
		failCopy        bool
	}{
		{"success", "api", false},
		{"quoted_path", "api ' $literal", false},
		{"copy_failure", "api", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failCopy := tc.failCopy
			dir := filepath.Join(t.TempDir(), tc.directory)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			live := filepath.Join(dir, "bitcal-api")
			backup := live + ".bak-fixed"
			for path, content := range map[string]string{live: "new binary", backup: "old binary"} {
				if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Link(live, live+".running"); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", function+`
set -euo pipefail
SSH_HOST='fixture-host'
BINARY=bitcal-api
SERVICE=bitcal-api
HEALTH_URL=http://fixture.invalid/health
BEFORE_VERSION=fixture-old
BACKUP="$REMOTE_API/$BINARY.bak-fixed"
ssh() { test "$1" = fixture-host; shift; bash -c "$*"; }
chown() { test "$1" = root:root; test "$2" != "$REMOTE_API/bitcal-api"; }
systemctl() { test "$*" = 'restart bitcal-api'; test "$(command cat "$REMOTE_API/bitcal-api")" = 'old binary'; touch "$REMOTE_API/restarted"; }
curl() { test -f "$REMOTE_API/restarted"; printf '%s\n' '{"status":"ok","version":"fixture-old"}'; }
jq() {
  test "$1" = -e
  test "$2" = --arg
  test "$3" = version
  test "$4" = fixture-old
  test "$5" = '.status == "ok" and .version == $version'
  test "$(command cat)" = '{"status":"ok","version":"fixture-old"}'
  touch "$REMOTE_API/verified"
}
cp() { if [ "$FAIL_COPY" = true ]; then return 1; fi; command cp "$@"; }
export -f ssh chown systemctl curl jq cp
manual_commands > "$REMOTE_API/instructions"
sed -n '/roll back by hand:/,/verify by hand:/{ /verify by hand:/d; s/^[[:space:]]*roll back by hand: *//; p; }' "$REMOTE_API/instructions" > "$REMOTE_API/recipe"
test -s "$REMOTE_API/recipe"
bash -e "$REMOTE_API/recipe"
`)
			failValue := "false"
			if failCopy {
				failValue = "true"
			}
			cmd.Env = append(os.Environ(), "REMOTE_API="+dir, "FAIL_COPY="+failValue)
			out, runErr := cmd.CombinedOutput()
			if (runErr != nil) != failCopy {
				t.Fatalf("recipe error = %v, want failure %v: %s", runErr, failCopy, out)
			}
			wantLive := "old binary"
			if failCopy {
				wantLive = "new binary"
			}
			for path, want := range map[string]string{live: wantLive, backup: "old binary", live + ".running": "new binary"} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", path, got, err, want)
				}
			}
			for _, marker := range []string{"restarted", "verified"} {
				_, err := os.Stat(filepath.Join(dir, marker))
				if (err == nil) == failCopy {
					t.Errorf("%s marker: %v (copy failed: %v)", marker, err, failCopy)
				}
			}
			info, err := os.Stat(live)
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Errorf("live binary permissions: %v (%v)", info, err)
			}
			leftovers, err := filepath.Glob(live + ".rollback.*")
			if err != nil || len(leftovers) != 0 {
				t.Errorf("temporary files left: %v (%v)", leftovers, err)
			}
		})
	}
}
