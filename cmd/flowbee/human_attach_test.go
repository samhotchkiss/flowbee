package main

import (
	"reflect"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/bootstrap"
)

type attachRunnerFake struct{ calls [][]string }

func (f *attachRunnerFake) Run(name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func TestHumanAttachEmitsOnlyAttachOrSwitchClientArgv(t *testing.T) {
	intent := bootstrap.AttachIntentSpec{TmuxServerDomainID: bootstrap.ExternalTmuxServerDomain,
		PresentationName: "russ-interactor"}
	for _, test := range []struct {
		inside bool
		want   []string
	}{{true, []string{"tmux", "switch-client", "-t", "russ-interactor"}},
		{false, []string{"tmux", "attach-session", "-t", "russ-interactor"}}} {
		fake := &attachRunnerFake{}
		domain := ""
		if test.inside {
			domain = bootstrap.ExternalTmuxServerDomain
		}
		if err := attachHumanToInteractor(intent, test.inside, domain, fake); err != nil {
			t.Fatal(err)
		}
		if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], test.want) {
			t.Fatalf("calls = %#v, want %#v", fake.calls, test.want)
		}
	}
}

func TestHumanAttachRejectsManagedDomainAndUnreservedNameWithoutExecutingTmux(t *testing.T) {
	for _, intent := range []bootstrap.AttachIntentSpec{
		{TmuxServerDomainID: bootstrap.ManagedTmuxServerDomain, PresentationName: "russ-interactor"},
		{TmuxServerDomainID: bootstrap.ExternalTmuxServerDomain, PresentationName: "russ"},
	} {
		fake := &attachRunnerFake{}
		if err := attachHumanToInteractor(intent, true, bootstrap.ExternalTmuxServerDomain, fake); err == nil {
			t.Fatal("unsafe attach intent was accepted")
		}
		if len(fake.calls) != 0 {
			t.Fatalf("unsafe intent executed tmux: %#v", fake.calls)
		}
	}
}

func TestHumanAttachRejectsCurrentManagedClientWithoutExecutingTmux(t *testing.T) {
	fake := &attachRunnerFake{}
	intent := bootstrap.AttachIntentSpec{TmuxServerDomainID: bootstrap.ExternalTmuxServerDomain,
		PresentationName: "russ-interactor"}
	if err := attachHumanToInteractor(intent, true, bootstrap.ManagedTmuxServerDomain, fake); err == nil {
		t.Fatal("managed current client was accepted")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("managed client executed tmux: %#v", fake.calls)
	}
}
