package detect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  []Runner
	}{
		{
			name:  "go",
			files: []string{"go.mod"},
			want:  []Runner{{Name: "go test", Command: "go", Args: []string{"test", "./..."}}},
		},
		{
			name:  "node",
			files: []string{"package.json"},
			want:  []Runner{{Name: "npm test", Command: "npm", Args: []string{"test"}}},
		},
		{
			name:  "python pyproject",
			files: []string{"pyproject.toml"},
			want:  []Runner{{Name: "pytest", Command: "pytest"}},
		},
		{
			name:  "python setup",
			files: []string{"setup.py"},
			want:  []Runner{{Name: "pytest", Command: "pytest"}},
		},
		{
			name:  "python requirements",
			files: []string{"requirements.txt"},
			want:  []Runner{{Name: "pytest", Command: "pytest"}},
		},
		{
			name:  "rust",
			files: []string{"Cargo.toml"},
			want:  []Runner{{Name: "cargo test", Command: "cargo", Args: []string{"test"}}},
		},
		{
			name:  "multiple markers are all reported in fixed order",
			files: []string{"go.mod", "package.json", "requirements.txt", "Cargo.toml"},
			want: []Runner{
				{Name: "go test", Command: "go", Args: []string{"test", "./..."}},
				{Name: "npm test", Command: "npm", Args: []string{"test"}},
				{Name: "pytest", Command: "pytest"},
				{Name: "cargo test", Command: "cargo", Args: []string{"test"}},
			},
		},
		{
			name:  "nothing recognized",
			files: []string{"README.md", "src/main.go"},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := Detect(dir)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("Detect() = %#v, want no runners", got)
				}
				if Recognized(dir) {
					t.Fatal("Recognized() = true, want false")
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Detect() = %#v, want %#v", got, tc.want)
			}
			if !Recognized(dir) {
				t.Fatal("Recognized() = false, want true")
			}
		})
	}
}

func TestRunnerString(t *testing.T) {
	r := Runner{Name: "go test", Command: "go", Args: []string{"test", "./..."}}
	if got, want := r.String(), "go test ./..."; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
