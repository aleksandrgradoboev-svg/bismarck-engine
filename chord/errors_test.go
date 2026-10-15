package chord

import (
	"errors"
	"slices"
	"testing"
)

// packages/chord/src/services/errors.ts at 64eeb82a4 fixes the eight codes
// that may cross a service boundary, in this order.
func TestRemoteServiceErrorCodesMatchUpstream(t *testing.T) {
	want := []RemoteServiceErrorCode{
		"service_not_allowed",
		"service_not_found",
		"service_mode_mismatch",
		"service_member_not_found",
		"service_member_mismatch",
		"service_instance_not_found",
		"service_stale_instance",
		"service_invalid_value",
	}
	if got := RemoteServiceErrorCodes(); !slices.Equal(got, want) {
		t.Errorf("codes = %q, want %q", got, want)
	}
	typed := []RemoteServiceErrorCode{
		ServiceNotAllowed, ServiceNotFound, ServiceModeMismatch, ServiceMemberNotFound,
		ServiceMemberMismatch, ServiceInstanceNotFound, ServiceStaleInstance, ServiceInvalidValue,
	}
	if !slices.Equal(typed, want) {
		t.Errorf("constants = %q, want %q", typed, want)
	}
}

// The code set is upstream's readonly tuple: a caller gets its own copy, and
// nothing it does to that copy widens what the recognizer accepts.
func TestRemoteServiceErrorCodesIsFixed(t *testing.T) {
	codes := RemoteServiceErrorCodes()
	codes[0] = "bogus"
	codes = append(codes, "bogus")
	if IsRemoteServiceErrorCode("bogus") {
		t.Error(`IsRemoteServiceErrorCode("bogus") = true after editing a returned copy`)
	}
	if !IsRemoteServiceErrorCode(string(ServiceNotAllowed)) {
		t.Errorf("%q no longer recognized after editing a returned copy", ServiceNotAllowed)
	}
	if got := RemoteServiceErrorCodes(); got[0] != ServiceNotAllowed || len(got) != 8 {
		t.Errorf("second call = %q, want the original eight", got)
	}
}

func TestIsRemoteServiceErrorCode(t *testing.T) {
	for _, code := range RemoteServiceErrorCodes() {
		if !IsRemoteServiceErrorCode(string(code)) {
			t.Errorf("%q not recognized", code)
		}
	}
	for _, s := range []string{"", "service", "SERVICE_NOT_FOUND", "service_not_found "} {
		if IsRemoteServiceErrorCode(s) {
			t.Errorf("%q recognized", s)
		}
	}
}

func TestRemoteServiceErrorCarriesCodeAndMessage(t *testing.T) {
	err := NewRemoteServiceError(ServiceNotFound, "Service test.models is not available")
	if err.Code != ServiceNotFound || err.Message != "Service test.models is not available" {
		t.Errorf("err = %+v", err)
	}
	if got := err.Error(); got != "Service test.models is not available" {
		t.Errorf("Error() = %q, want the message alone, as Error.message is upstream", got)
	}
	var target *RemoteServiceError
	wrapped := errors.Join(errors.New("other"), err)
	if !errors.As(wrapped, &target) || target != err {
		t.Error("errors.As did not find the RemoteServiceError")
	}
}
