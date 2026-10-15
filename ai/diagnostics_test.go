package ai

import (
	"encoding/json"
	"testing"
)

// TestDiagnosticMarshalMatchesPi asserts the JSON shape matches pi's
// AssistantMessageDiagnostic (utils/diagnostics.ts): {type, timestamp, error?, details?}
// with error = {name?, message, stack?, code?}.
func TestDiagnosticMarshalMatchesPi(t *testing.T) {
	d := Diagnostic{
		Type:      "stream_error",
		Timestamp: 1717000000000,
		Error: &DiagnosticErrorInfo{
			Name:    "TypeError",
			Message: "boom",
			Stack:   "at foo",
			Code:    "ECONN",
		},
		Details: OrderedObject{{Key: "attempt", Value: float64(2)}},
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"stream_error","timestamp":1717000000000,"error":{"name":"TypeError","message":"boom","stack":"at foo","code":"ECONN"},"details":{"attempt":2}}`
	if string(raw) != want {
		t.Fatalf("diagnostic JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestDiagnosticMarshalMinimal asserts optional fields drop and message is always
// present, matching pi's optional error/details and required message.
func TestDiagnosticMarshalMinimal(t *testing.T) {
	d := Diagnostic{
		Type:      "retry",
		Timestamp: 100,
		Error:     &DiagnosticErrorInfo{Message: "x"},
	}
	raw, _ := json.Marshal(d)
	want := `{"type":"retry","timestamp":100,"error":{"message":"x"}}`
	if string(raw) != want {
		t.Fatalf("minimal diagnostic JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestDiagnosticErrorCodeNumber confirms code round-trips as a number too
// (pi: code?: string | number).
func TestDiagnosticErrorCodeNumber(t *testing.T) {
	d := Diagnostic{
		Type:      "http",
		Timestamp: 1,
		Error:     &DiagnosticErrorInfo{Message: "bad", Code: float64(429)},
	}
	raw, _ := json.Marshal(d)
	want := `{"type":"http","timestamp":1,"error":{"message":"bad","code":429}}`
	if string(raw) != want {
		t.Fatalf("numeric code JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestDiagnosticDetailsKeepPiOrder requires details to be written in their
// own key order, as JSON.stringify writes pi's details object, an empty
// details object to be written {} as pi writes it, and nil (pi's undefined)
// to be omitted; and a diagnostic read back to write the same text.
func TestDiagnosticDetailsKeepPiOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details OrderedObject
		want    string
	}{
		{name: "ordered", details: OrderedObject{{Key: "z", Value: 1}, {Key: "a", Value: OrderedObject{{Key: "y", Value: 2}, {Key: "b", Value: 3}}}}, want: `{"type":"t","timestamp":1,"details":{"z":1,"a":{"y":2,"b":3}}}`},
		{name: "empty", details: OrderedObject{}, want: `{"type":"t","timestamp":1,"details":{}}`},
		{name: "undefined", details: nil, want: `{"type":"t","timestamp":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(Diagnostic{Type: "t", Timestamp: 1, Details: tc.details})
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want {
				t.Fatalf("marshal = %s, want %s", raw, tc.want)
			}
			var back Diagnostic
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			again, err := json.Marshal(back)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != tc.want {
				t.Fatalf("read back and marshaled = %s, want %s", again, tc.want)
			}
		})
	}
}
