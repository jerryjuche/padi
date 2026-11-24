package risk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
)

func gitTest(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := gitops.Run(ctx, dir, env, append([]string{"-C", dir}, args...)...)
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, nil, "init", "-q")
	gitTest(t, dir, nil, "config", "user.name", "Fixture")
	gitTest(t, dir, nil, "config", "user.email", "fixture@example.com")
	return dir
}

func fixtureCommit(t *testing.T, dir, rel, body, message, author string, when time.Time) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, nil, "add", "--", rel)
	date := when.UTC().Format(time.RFC3339)
	env := []string{
		"GIT_AUTHOR_NAME=" + author,
		"GIT_AUTHOR_EMAIL=" + author + "@example.com",
		"GIT_COMMITTER_NAME=" + author,
		"GIT_COMMITTER_EMAIL=" + author + "@example.com",
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_DATE=" + date,
	}
	gitTest(t, dir, env, "commit", "-q", "-m", message)
}

func fixtureCommitFiles(t *testing.T, dir, message, author string, when time.Time, contents map[string]string) {
	t.Helper()
	for rel, body := range contents {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"add", "--"}
	for rel := range contents {
		args = append(args, rel)
	}
	gitTest(t, dir, nil, args...)
	date := when.UTC().Format(time.RFC3339)
	env := []string{
		"GIT_AUTHOR_NAME=" + author, "GIT_AUTHOR_EMAIL=" + author + "@example.com",
		"GIT_COMMITTER_NAME=" + author, "GIT_COMMITTER_EMAIL=" + author + "@example.com",
		"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date,
	}
	gitTest(t, dir, env, "commit", "-q", "-m", message)
}

func daysAgo(n int) time.Time { return time.Now().UTC().AddDate(0, 0, -n) }

func baseFile(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

type riskFixture struct {
	setup      func(t *testing.T) (dir, base string, files []File, incidents []IncidentRef)
	want       Level
	wantReason string
}

func TestForDiff(t *testing.T) {
	path := "internal/cache/store.go"

	cases := []riskFixture{
		{
			want: LevelHigh, wantReason: "fix density",
			setup: func(t *testing.T) (string, string, []File, []IncidentRef) {
				dir := newFixtureRepo(t)
				fixtureCommit(t, dir, path, baseFile(10), "add cache store", "alice", daysAgo(100))
				fixtureCommit(t, dir, path, baseFile(10)+"one\n", "fix cache eviction", "bob", daysAgo(10))
				fixtureCommit(t, dir, path, baseFile(10)+"one\ntwo\n", "fixes cache race", "bob", daysAgo(8))
				fixtureCommit(t, dir, path, baseFile(10)+"one\ntwo\nthree\n", "hotfix cache flush", "carol", daysAgo(6))
				base := strings.TrimSpace(gitTest(t, dir, nil, "rev-parse", "HEAD~3"))
				if err := os.WriteFile(filepath.Join(dir, path), []byte(baseFile(10)+"one\ntwo\nthree\nfour\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return dir, base, []File{{Path: path}}, nil
			},
		},
		{
			want: LevelMedium, wantReason: "stale",
			setup: func(t *testing.T) (string, string, []File, []IncidentRef) {
				dir := newFixtureRepo(t)
				fixtureCommit(t, dir, "docs/notes.txt", baseFile(10), "add notes", "alice", daysAgo(400))
				base := strings.TrimSpace(gitTest(t, dir, nil, "rev-parse", "HEAD"))
				if err := os.WriteFile(filepath.Join(dir, "docs/notes.txt"), []byte(baseFile(13)), 0o600); err != nil {
					t.Fatal(err)
				}
				return dir, base, []File{{Path: "docs/notes.txt"}}, nil
			},
		},
		{
			want: LevelMedium, wantReason: "co-change",
			setup: func(t *testing.T) (string, string, []File, []IncidentRef) {
				dir := newFixtureRepo(t)
				index := "internal/cache/index.go"
				fixtureCommitFiles(t, dir, "add cache pair", "alice", daysAgo(100), map[string]string{
					path: baseFile(10), index: baseFile(10),
				})
				fixtureCommitFiles(t, dir, "update cache pair", "alice", daysAgo(10), map[string]string{
					path: baseFile(10) + "one\n", index: baseFile(10) + "one\n",
				})
				base := strings.TrimSpace(gitTest(t, dir, nil, "rev-parse", "HEAD~1"))
				if err := os.WriteFile(filepath.Join(dir, path), []byte(baseFile(10)+"one\ntwo\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return dir, base, []File{{Path: path}}, nil
			},
		},
		{
			want: LevelMedium, wantReason: "incident",
			setup: func(t *testing.T) (string, string, []File, []IncidentRef) {
				dir, base, files, _ := lowRiskFixture(t, path)
				incidents := []IncidentRef{{
					ID: uuid.New(), Title: "outage in " + path, OccurredAt: daysAgo(5),
				}}
				return dir, base, files, incidents
			},
		},
		{
			want: LevelHigh, wantReason: "incidents",
			setup: func(t *testing.T) (string, string, []File, []IncidentRef) {
				dir, base, files, _ := lowRiskFixture(t, path)
				incidents := []IncidentRef{
					{ID: uuid.New(), Title: "outage in " + path, OccurredAt: daysAgo(5)},
					{ID: uuid.New(), Title: "store.go regression", OccurredAt: daysAgo(3)},
				}
				return dir, base, files, incidents
			},
		},
		{
			want: LevelUnknown, wantReason: "history unavailable",
			setup: func(t *testing.T) (string, string, []File, []IncidentRef) {
				dir := newFixtureRepo(t)
				fixtureCommit(t, dir, path, baseFile(10), "add cache store", "alice", daysAgo(20))
				base := strings.TrimSpace(gitTest(t, dir, nil, "rev-parse", "HEAD"))
				return dir, base, []File{{Path: "bad;name.go"}}, nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.want)+"/"+tc.wantReason, func(t *testing.T) {
			dir, base, files, incidents := tc.setup(t)
			a := New(Thresholds{}).WithIncidents(incidents)
			risks, level, err := a.ForDiff(context.Background(), dir, base, files)
			if err != nil {
				t.Fatal(err)
			}
			if level != tc.want {
				t.Fatalf("aggregate = %q, want %q (risks: %+v)", level, tc.want, risks)
			}
			if len(risks) != 1 {
				t.Fatalf("got %d file risks, want 1", len(risks))
			}
			if risks[0].Level != tc.want {
				t.Fatalf("file level = %q, want %q", risks[0].Level, tc.want)
			}
			if !strings.Contains(strings.Join(risks[0].Reasons, " "), tc.wantReason) {
				t.Fatalf("reasons %q do not mention %q", risks[0].Reasons, tc.wantReason)
			}
		})
	}
}

func lowRiskFixture(t *testing.T, path string) (string, string, []File, []IncidentRef) {
	t.Helper()
	dir := newFixtureRepo(t)
	fixtureCommit(t, dir, path, baseFile(10), "add cache store", "alice", daysAgo(20))
	base := strings.TrimSpace(gitTest(t, dir, nil, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(dir, path), []byte(baseFile(10)+"one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, base, []File{{Path: path}}, nil
}

func TestForDiffIncidentWindowAndConfirmation(t *testing.T) {
	path := "internal/cache/store.go"
	dir, base, files, _ := lowRiskFixture(t, path)
	old := []IncidentRef{{ID: uuid.New(), Title: "outage in " + path, OccurredAt: daysAgo(400)}}
	future := []IncidentRef{{ID: uuid.New(), Title: "outage in " + path, OccurredAt: time.Now().UTC().AddDate(0, 0, 2)}}

	for label, incidents := range map[string][]IncidentRef{"old": old, "future": future} {
		a := New(Thresholds{}).WithIncidents(incidents)
		_, level, err := a.ForDiff(context.Background(), dir, base, files)
		if err != nil {
			t.Fatal(err)
		}
		if level != LevelLow {
			t.Fatalf("%s incident changed level to %q", label, level)
		}
	}
	// WithIncidents must not mutate the receiver.
	a := New(Thresholds{})
	_, level, err := a.ForDiff(context.Background(), dir, base, files)
	if err != nil {
		t.Fatal(err)
	}
	if level != LevelLow {
		t.Fatalf("base level = %q, want low", level)
	}
}

func TestForDiffInvalidQuarantineDir(t *testing.T) {
	a := New(Thresholds{})
	if _, _, err := a.ForDiff(context.Background(), filepath.Join(t.TempDir(), "missing"), "HEAD", []File{{Path: "a.go"}}); err == nil {
		t.Fatal("expected error for invalid quarantine directory")
	}
}
