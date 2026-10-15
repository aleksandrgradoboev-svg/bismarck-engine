package chord

import "testing"

// The defineService assertions in packages/chord/test/services.test.ts at
// 64eeb82a4 ("marks services remotable by default and reserves Chord service
// IDs", "checks remote JSON contracts only at compile time") pin what
// DefineService and DefineLocalService return. The rest of that file is the
// provider and replicated-state runtime, ported with those slices.

type models interface{ Select(name string) error }

func TestDefineServiceMarksServicesRemotableByDefault(t *testing.T) {
	remote := DefineService[models]("test.models")
	local := DefineLocalService[struct{ Value string }]("test.local")
	if remote.ID() != "test.models" || remote.Local() {
		t.Errorf("remote = %+v, want id test.models, local false", remote)
	}
	if local.ID() != "test.local" || !local.Local() {
		t.Errorf("local = %+v, want id test.local, local true", local)
	}
	if DefineService[any]("test.json-passthrough").Local() {
		t.Error("remote service reported local")
	}
}

func TestDefineServiceReservesChordServiceIDs(t *testing.T) {
	// Upstream tests id.startsWith("$chord."): the namespace is reserved only
	// at the start, so an ID that merely contains it is an ordinary ID.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("\"app.$chord.internal\": panic %q, want accepted", r)
			}
		}()
		if got := DefineService[any]("app.$chord.internal").ID(); got != "app.$chord.internal" {
			t.Errorf("ID = %q, want app.$chord.internal", got)
		}
	}()
	for _, tc := range []struct{ id, want string }{
		{"$chord.internal", "Service IDs beginning with $chord. are reserved"},
		{"", "Service ID must not be empty"},
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("%q: no panic", tc.id)
					return
				}
				if msg, _ := r.(string); msg != "chord: "+tc.want {
					t.Errorf("%q: panic %q, want %q", tc.id, r, "chord: "+tc.want)
				}
			}()
			DefineLocalService[any](tc.id)
		}()
	}
}

// A service token is its ID: two definitions of the same ID and locality are
// the same map key, the way upstream's provider tables key on service.id.
func TestServiceIsComparableByID(t *testing.T) {
	a := DefineService[models]("test.models")
	b := DefineService[models]("test.models")
	if a != b {
		t.Error("same id and locality compare unequal")
	}
	if got := a.String(); got != "test.models" {
		t.Errorf("String() = %q", got)
	}
}
