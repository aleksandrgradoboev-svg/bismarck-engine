package ai

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// abortRaceTimeout is how long an aborted caller may take to be released. pi
// rejects immediately; anything approaching this means the race is missing.
const abortRaceTimeout = 500 * time.Millisecond

// uncooperativeProvider returns a provider whose Resolve blocks forever and
// ignores the ctx it is handed — the documented case, since ApiKeyAuth.Resolve
// may execute commands. entered is closed once Resolve is running, so a test can
// cancel while the call is genuinely in flight.
func uncooperativeProvider(id string, entered chan<- struct{}, release <-chan struct{}) Provider {
	var once atomic.Bool
	return CreateProvider(CreateProviderOptions{
		ID: id,
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "uncooperative",
			Resolve: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthResult, error) {
				if once.CompareAndSwap(false, true) {
					close(entered)
				}
				<-release
				return &AuthResult{Auth: ModelAuth{APIKey: "k"}, Source: "src"}, nil
			},
		}},
		Models: []*Model{{Provider: id, ID: "m"}},
		API:    stubAPI(),
	})
}

// TestCheckAuthReleasesAbortedCaller locks pi's raceWithAbortSignal at
// models.ts:502: an aborted caller is rejected immediately even when the
// provider's resolve ignores its signal.
func TestCheckAuthReleasesAbortedCaller(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	m := CreateModels(nil)
	m.SetProvider(uncooperativeProvider("stubborn", entered, release))

	ctx, cancel := context.WithCancel(context.Background())
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := m.CheckAuth(ctx, "stubborn")
		done <- result{err}
	}()
	<-entered
	cancel()

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("CheckAuth err = %v, want context.Canceled", r.err)
		}
	case <-time.After(abortRaceTimeout):
		t.Fatal("CheckAuth did not return after cancel; the abort race is missing")
	}
}

// TestGetAvailableReleasesAbortedCaller is the same guarantee at models.ts:524.
func TestGetAvailableReleasesAbortedCaller(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	m := CreateModels(nil)
	m.SetProvider(uncooperativeProvider("stubborn", entered, release))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.GetAvailable(ctx, "")
		done <- err
	}()
	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("GetAvailable err = %v, want context.Canceled", err)
		}
	case <-time.After(abortRaceTimeout):
		t.Fatal("GetAvailable did not return after cancel; the abort race is missing")
	}
}

// TestLoginReleasesAbortedCaller is the same guarantee at models.ts:558, where
// pi races the login operation itself.
func TestLoginReleasesAbortedCaller(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	p := CreateProvider(CreateProviderOptions{
		ID: "stubborn",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "uncooperative",
			Login: func(_ context.Context, _ AuthInteraction) (*Credential, error) {
				close(entered)
				<-release
				return &Credential{Type: CredentialAPIKey, Key: "k"}, nil
			},
		}},
		Models: []*Model{{Provider: "stubborn", ID: "m"}},
		API:    stubAPI(),
	})
	m := CreateModels(nil)
	m.SetProvider(p)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Login(ctx, "stubborn", CredentialAPIKey, fakeInteraction{}, nil)
		done <- err
	}()
	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Login err = %v, want context.Canceled", err)
		}
	case <-time.After(abortRaceTimeout):
		t.Fatal("Login did not return after cancel; the abort race is missing")
	}
}

// TestGetAvailableChecksEveryProvider locks pi's Promise.all fan-out
// (models.ts:524): every provider's check is invoked even when an earlier one
// fails. A sequential loop stopped at the first error and never asked the rest.
func TestGetAvailableChecksEveryProvider(t *testing.T) {
	failing := CreateProvider(CreateProviderOptions{
		ID: "a-failing",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "failing",
			Check: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthCheck, error) {
				return nil, errors.New("check exploded")
			},
		}},
		Models: []*Model{{Provider: "a-failing", ID: "m"}},
		API:    stubAPI(),
	})

	var laterChecked atomic.Bool
	later := CreateProvider(CreateProviderOptions{
		ID: "b-later",
		Auth: ProviderAuth{APIKey: &ApiKeyAuth{
			Name: "later",
			Check: func(_ context.Context, _ AuthContext, _ *Credential) (*AuthCheck, error) {
				laterChecked.Store(true)
				return &AuthCheck{Source: "env", Type: CredentialAPIKey}, nil
			},
		}},
		Models: []*Model{{Provider: "b-later", ID: "m"}},
		API:    stubAPI(),
	})

	m := CreateModels(nil)
	m.SetProvider(failing)
	m.SetProvider(later)

	if _, err := m.GetAvailable(context.Background(), ""); err == nil {
		t.Fatal("GetAvailable should surface the failing provider's error")
	}
	if !laterChecked.Load() {
		t.Fatal("provider after the failing one was never checked; pi's Promise.all invokes all")
	}
}
