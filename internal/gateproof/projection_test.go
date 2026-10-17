package gateproof_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/gateproof"
)

// candidateRepository creates a Git checkout with one commit and then the
// uncommitted state an onboarding agent leaves: a modified tracked file, a new
// untracked file, a deleted tracked file, ignored build output, a local
// credential file, and a leftover proof directory.
func candidateRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	git(t, repository, "init", "-q")
	writeFile(t, repository, "README.md", "committed\n", 0o644)
	writeFile(t, repository, "scripts/check.sh", "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, repository, "removed.txt", "gone soon\n", 0o644)
	writeFile(t, repository, ".gitignore", "node_modules/\n", 0o644)
	git(t, repository, "add", "-A")
	git(t, repository, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "initial")

	writeFile(t, repository, "README.md", "changed by onboarding\n", 0o644)
	writeFile(t, repository, "factory.yaml", "schema_version: 1\n", 0o644)
	if err := os.Remove(filepath.Join(repository, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repository, "node_modules/left-pad/index.js", "ignored\n", 0o644)
	writeFile(t, repository, ".env", "TOKEN=secret\n", 0o600)
	writeFile(t, repository, ".factory-proof.abc123/output", "old proof\n", 0o644)
	return repository
}

// git runs one Git command in directory and fails the test on error. It
// turns off automatic maintenance, so no detached Git process writes to the
// repository after the command returns.
func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory, "-c", "maintenance.auto=false"}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

// writeFile creates one file and its parents below root.
func writeFile(t *testing.T, root, relative, contents string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// checkoutState captures the status and file list of a checkout, so a test
// can prove a projection left it unchanged.
func checkoutState(t *testing.T, repository string) string {
	t.Helper()
	var files []string
	if err := filepath.Walk(repository, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(repository, path)
		// Git maintenance runs in the background after a commit and adds and
		// removes lock files below .git, so the walk must not enter it.
		if relative == ".git" && info.IsDir() {
			return filepath.SkipDir
		}
		files = append(files, relative+" "+info.Mode().String())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return git(t, repository, "status", "--porcelain", "--ignored") + strings.Join(files, "\n")
}

// TestProjectCopiesTheCandidateFilesOnly verifies the projection holds the
// candidate state, with uncommitted changes, and leaves out Git internals,
// ignored output, local credential files, and Factory artifacts.
func TestProjectCopiesTheCandidateFilesOnly(t *testing.T) {
	repository := candidateRepository(t)
	before := checkoutState(t, repository)
	destination := t.TempDir()

	candidate, err := gateproof.Project(context.Background(), repository, destination)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	for relative, want := range map[string]string{
		"README.md":    "changed by onboarding\n",
		"factory.yaml": "schema_version: 1\n",
		".gitignore":   "node_modules/\n",
	} {
		got, err := os.ReadFile(filepath.Join(destination, relative))
		if err != nil || string(got) != want {
			t.Errorf("projected %s = %q, %v; want %q", relative, got, err, want)
		}
	}
	info, err := os.Stat(filepath.Join(destination, "scripts", "check.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("projected scripts/check.sh mode = %v, %v; want it executable", info, err)
	}
	for _, absent := range []string{".git", "removed.txt", "node_modules", ".env", ".factory-proof.abc123"} {
		if _, err := os.Lstat(filepath.Join(destination, absent)); !os.IsNotExist(err) {
			t.Errorf("projection contains %s (stat error %v)", absent, err)
		}
	}
	if candidate.Files != 4 {
		t.Errorf("Files = %d, want 4", candidate.Files)
	}
	wantExcluded := map[string]gateproof.ExclusionReason{".env": "credential_file", ".factory-proof.abc123/output": "factory_artifact"}
	if len(candidate.Excluded) != len(wantExcluded) {
		t.Errorf("Excluded = %#v, want %v", candidate.Excluded, wantExcluded)
	}
	for _, excluded := range candidate.Excluded {
		if wantExcluded[excluded.Path] != excluded.Reason {
			t.Errorf("excluded %#v, want %v", excluded, wantExcluded)
		}
	}
	if !strings.HasPrefix(candidate.Fingerprint, "sha256:") || len(candidate.Fingerprint) != len("sha256:")+64 {
		t.Errorf("Fingerprint = %q, want a sha256 digest", candidate.Fingerprint)
	}
	if after := checkoutState(t, repository); after != before {
		t.Errorf("projection changed the live checkout:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestProjectFingerprintFollowsTheCandidateContents verifies the same
// candidate gives the same fingerprint and any file change gives another.
func TestProjectFingerprintFollowsTheCandidateContents(t *testing.T) {
	repository := candidateRepository(t)
	project := func() string {
		t.Helper()
		candidate, err := gateproof.Project(context.Background(), repository, t.TempDir())
		if err != nil {
			t.Fatalf("Project() error = %v", err)
		}
		return candidate.Fingerprint
	}

	first := project()
	if again := project(); again != first {
		t.Fatalf("fingerprint changed without a file change: %s then %s", first, again)
	}
	writeFile(t, repository, "factory.yaml", "schema_version: 1\nsetup: true\n", 0o644)
	if changed := project(); changed == first {
		t.Fatal("fingerprint did not change after factory.yaml changed")
	}
}

// TestProjectLeavesOutAnUntrackedCopilotToken verifies a local copy of the
// dedicated Copilot token never reaches the proof worker.
func TestProjectLeavesOutAnUntrackedCopilotToken(t *testing.T) {
	repository := candidateRepository(t)
	writeFile(t, repository, "local/copilot-token", "github_pat_test-only\n", 0o600)
	destination := t.TempDir()

	candidate, err := gateproof.Project(context.Background(), repository, destination)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "local", "copilot-token")); !os.IsNotExist(err) {
		t.Fatalf("projection contains the Copilot token (stat error %v)", err)
	}
	for _, excluded := range candidate.Excluded {
		if excluded.Path == "local/copilot-token" && excluded.Reason == gateproof.ExcludedCredentialFile {
			return
		}
	}
	t.Fatalf("Excluded = %#v, want local/copilot-token as a credential file", candidate.Excluded)
}

// TestProjectRefusesADirectoryThatIsNotACheckout verifies the candidate must
// be a Git checkout, because Git selects the candidate files.
func TestProjectRefusesADirectoryThatIsNotACheckout(t *testing.T) {
	if _, err := gateproof.Project(context.Background(), t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("Project() error = nil, want a refusal outside a Git checkout")
	}
}

// TestProjectRunsNoRepositoryFsmonitor verifies the candidate listing applies
// the factory Git policy, so a repository fsmonitor command cannot run on the
// host.
func TestProjectRunsNoRepositoryFsmonitor(t *testing.T) {
	repository := candidateRepository(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nprintf '/\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "config", "core.fsmonitor", hook)

	if _, err := gateproof.Project(context.Background(), repository, t.TempDir()); err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the repository fsmonitor ran on the host (stat error %v)", err)
	}
}

// TestProjectRefusesFilesBehindASymlinkedDirectory verifies a tracked path
// whose parent directory became a symbolic link to a host directory is not
// copied, so the projection never reads data outside the checkout.
func TestProjectRefusesFilesBehindASymlinkedDirectory(t *testing.T) {
	repository := candidateRepository(t)
	writeFile(t, repository, "lib/a.txt", "committed\n", 0o644)
	git(t, repository, "add", "lib/a.txt")
	git(t, repository, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "lib")
	host := t.TempDir()
	writeFile(t, host, "a.txt", "host secret\n", 0o600)
	if err := os.RemoveAll(filepath.Join(repository, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(repository, "lib")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repository, ".gitignore", "node_modules/\nlib\n", 0o644)
	destination := t.TempDir()

	candidate, err := gateproof.Project(context.Background(), repository, destination)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	if data, err := os.ReadFile(filepath.Join(destination, "lib", "a.txt")); err == nil {
		t.Fatalf("projection copied %q from outside the checkout", data)
	}
	found := false
	for _, excluded := range candidate.Excluded {
		found = found || (excluded.Path == "lib/a.txt" && excluded.Reason == gateproof.ExcludedOutsideCheckout)
	}
	if !found {
		t.Fatalf("Excluded = %#v, want lib/a.txt as outside_checkout", candidate.Excluded)
	}
}

// TestFingerprintIdentifiesTheCandidateWithoutCopying verifies that the
// fingerprint equals the projection identity, writes nothing, and ignores
// the omitted paths.
func TestFingerprintIdentifiesTheCandidateWithoutCopying(t *testing.T) {
	repository := candidateRepository(t)
	before := checkoutState(t, repository)
	candidate, err := gateproof.Project(context.Background(), repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := gateproof.Fingerprint(context.Background(), repository)
	if err != nil || fingerprint != candidate.Fingerprint {
		t.Fatalf("fingerprint = %q, %v; want %q", fingerprint, err, candidate.Fingerprint)
	}
	if after := checkoutState(t, repository); after != before {
		t.Fatalf("the fingerprint changed the checkout:\n%s\nwant\n%s", after, before)
	}
	omitted, err := gateproof.Fingerprint(context.Background(), repository, "factory.yaml")
	if err != nil || omitted == fingerprint {
		t.Fatalf("omitted fingerprint = %q, %v; want another identity", omitted, err)
	}
	writeFile(t, repository, "factory.yaml", "schema_version: 2\n", 0o644)
	if changed, err := gateproof.Fingerprint(context.Background(), repository, "factory.yaml"); err != nil || changed != omitted {
		t.Fatalf("a change of an omitted path changed the fingerprint: %q, %v", changed, err)
	}
}
