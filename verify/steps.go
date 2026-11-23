package verify

import "time"

// GoBuild returns a step that runs `go build ./...`.
func GoBuild() *CommandStep {
	return Command("go_build", "go", "build", "./...").WithTimeout(2 * time.Minute)
}

// GoVet returns a step that runs `go vet ./...`.
func GoVet() *CommandStep {
	return Command("go_vet", "go", "vet", "./...").WithTimeout(3 * time.Minute)
}

// GoTest returns a step that runs `go test ./...`.
func GoTest() *CommandStep {
	return Command("go_test", "go", "test", "./...").WithTimeout(10 * time.Minute)
}

// TscCheck returns a step that runs `tsc --noEmit`. The directory it runs in
// must contain tsconfig.json.
func TscCheck() *CommandStep {
	return Command("tsc_check", "tsc", "--noEmit").WithTimeout(3 * time.Minute)
}

// JSONLint returns a step that checks path is well-formed JSON with `jq empty`.
func JSONLint(path string) *CommandStep {
	return Command("json_lint", "jq", "empty", path).WithTimeout(30 * time.Second)
}
