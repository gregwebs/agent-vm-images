package gotool

// The real runner, in one file so the import boundary (`os/exec`) is one reviewed
// entry rather than a capability spread through the package.

import (
	"errors"
	"os/exec"
	"strings"
)

// runProcess starts one child with exactly the program, argv, environment and
// directory the invocation names.
func runProcess(child Child) (Result, error) {
	command := exec.Command(child.Program, child.Argv...)
	command.Env = child.Env
	command.Dir = child.Dir
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.Status = exit.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}
