package main

import (
	"errors"
	"os"
	"os/exec"
	"regexp"

	"github.com/samhotchkiss/flowbee/internal/bootstrap"
)

var presentationNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}-interactor$`)

type tmuxAttachRunner interface {
	Run(name string, args ...string) error
}

type osTmuxAttachRunner struct{}

func (osTmuxAttachRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// attachHumanToInteractor is the sole raw tmux exception: presentation-only
// human UI attachment on the external/default server. The name is never used as
// lifecycle or routing authority, and this function cannot create a session or
// insert terminal input.
func attachHumanToInteractor(intent bootstrap.AttachIntentSpec, insideTmux bool, currentDomain string,
	runner tmuxAttachRunner) error {
	if runner == nil || intent.TmuxServerDomainID != bootstrap.ExternalTmuxServerDomain ||
		!presentationNamePattern.MatchString(intent.PresentationName) {
		return errors.New("human attach requires the reserved Interactor name on the default server")
	}
	if insideTmux {
		if currentDomain != bootstrap.ExternalTmuxServerDomain {
			return errors.New("current tmux client is not proven on the external/default server")
		}
		return runner.Run("tmux", "switch-client", "-t", intent.PresentationName)
	}
	return runner.Run("tmux", "attach-session", "-t", intent.PresentationName)
}
