package broker

import (
	"strings"

	"aigate/internal/registrymsg"
)

// Project templates (decision D8). The file texts are code constants; the only
// thing substituted is the project name, and only after it passed the name
// rule (^[a-z][a-z0-9-]{1,39}$), so no request text ever becomes file content.
// The description a user types goes to GitLab's project description field,
// never into a file.

// File is one template file (the GitLab client's CommitFile).
type File = CommitFile

const nameMarker = "@@NAME@@"

type tmplFile struct{ path, content string }

// Order matters: README.md is first because it creates `main` on the empty
// project (see GitLab.CommitFiles).
var templateSets = map[string][]tmplFile{
	"basic": {
		{"README.md", "# " + nameMarker + "\n\nA project created by aigate.\n\nThis template has no checks: it is not a target for automatic merging.\nAdd checks to `.aigate.yml` before relying on the gate.\n"},
		{".gitignore", ".DS_Store\n*.log\n*.tmp\n"},
		{".aigate.yml", "version: 1\nprotected:\n  - \".aigate.yml\"\nchecks: []\n"},
	},
	"go": {
		{"README.md", "# " + nameMarker + "\n\nA Go project created by aigate.\n\n    go vet ./...\n    go test ./...\n    go build ./...\n"},
		{".gitignore", "/" + nameMarker + "\n*.test\n*.out\n.DS_Store\n"},
		{"go.mod", "module " + nameMarker + "\n\ngo 1.21\n"},
		{"main.go", "package main\n\nimport \"fmt\"\n\n// Greeting is the text main prints.\nfunc Greeting() string {\n\treturn \"hello\"\n}\n\nfunc main() {\n\tfmt.Println(Greeting())\n}\n"},
		{"main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestGreeting(t *testing.T) {\n\tif Greeting() != \"hello\" {\n\t\tt.Fatal(\"unexpected greeting\")\n\t}\n}\n"},
		{".aigate.yml", "version: 1\nprotected:\n  - \"**/*_test.go\"\n  - \"go.mod\"\n  - \".aigate.yml\"\nchecks:\n" +
			"  - id: unit\n    preset: go\n    required: true\n" +
			"  - id: vet\n    adapter: exit-code\n    command: go vet ./...\n    required: true\n" +
			"  - id: build\n    adapter: exit-code\n    command: go build ./...\n    required: true\n"},
	},
}

// TemplateFiles returns the files of template for project name, README.md
// first. It returns nil for an unknown template or a name that does not fit
// the name rule.
func TemplateFiles(template, name string) []File {
	set, ok := templateSets[template]
	if !ok || !registrymsg.ValidName(name) {
		return nil
	}
	out := make([]File, 0, len(set))
	for _, f := range set {
		out = append(out, File{Path: f.path, Content: strings.ReplaceAll(f.content, nameMarker, name)})
	}
	return out
}
