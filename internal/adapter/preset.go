package adapter

// Preset is a ready-made check command. Command argv strings may contain the
// literal ${AIGATE_JUNIT}, which the runner replaces with the junit output
// path. Neutralize holds extra args that defeat repo options narrowing the
// test selection (e.g. pytest addopts). Adapter is "junit" or "gotest-json".
type Preset struct {
	Command    []string
	Neutralize []string
	Adapter    string
}

// PresetFor returns a copy of the named preset (pytest|go|vitest|jest).
//
// jest-junit only takes its output path from the JEST_JUNIT_OUTPUT_FILE
// environment variable and the jest-junit package must be installed in the
// project; the preset therefore sets it through env(1) in the argv.
func PresetFor(name string) (Preset, bool) {
	var p Preset
	switch name {
	case "pytest":
		p = Preset{
			// -P (Python 3.11+): do not prepend the cwd to sys.path, so a
			// pytest.py / pytest/ committed at the repo root cannot stand in
			// for the real runner. Repos importing from the root need
			// `pythonpath = .` in their pytest config.
			Command:    []string{"python", "-P", "-m", "pytest", "-q", "--junitxml=${AIGATE_JUNIT}"},
			Neutralize: []string{"-o", "addopts="},
			Adapter:    "junit",
		}
	case "go":
		p = Preset{Command: []string{"go", "test", "-json", "-count=1", "./..."}, Adapter: "gotest-json"}
	case "vitest":
		p = Preset{Command: []string{"npx", "--no-install", "vitest", "run", "--reporter=junit", "--outputFile=${AIGATE_JUNIT}"}, Adapter: "junit"}
	case "jest":
		p = Preset{Command: []string{"env", "JEST_JUNIT_OUTPUT_FILE=${AIGATE_JUNIT}", "npx", "--no-install", "jest", "--ci", "--reporters=jest-junit"}, Adapter: "junit"}
	default:
		return Preset{}, false
	}
	return p, true
}
