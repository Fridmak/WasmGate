package abi4

import "testing"

func TestPlanValidate(t *testing.T) {
	valid := []Plan{
		InspectRequestHeaders,
		InspectRequestHeaders | InspectRequestBody,
		InspectRequestHeaders | InspectResponseHeaders,
		InspectRequestHeaders | InspectResponseHeaders | InspectResponseBody,
		InspectOpaqueStream,
		InspectLifecycle,
	}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Fatalf("plan %b rejected: %v", p, err)
		}
	}
	invalid := []Plan{
		0,
		InspectOpaqueStream | InspectRequestHeaders,
		InspectRequestBody,
		InspectResponseBody,
		InspectLifecycle | (1 << 7),
	}
	for _, p := range invalid {
		if err := p.Validate(); err == nil {
			t.Fatalf("plan %b accepted", p)
		}
	}
}

func TestPlanRequiredCaps(t *testing.T) {
	p := InspectRequestHeaders | InspectResponseHeaders | InspectRequestBody
	got := p.RequiredCaps()
	want := CapBlob | CapHeaderIndex | CapStartLine
	if got != want {
		t.Fatalf("caps = %b, want %b", got, want)
	}
	if (CapBlob | CapHeaderIndex).Has(got) {
		t.Fatal("Has reported capabilities that are not present")
	}
	if got := (p | InspectLifecycle).RequiredCaps(); got != want|CapLifecycle {
		t.Fatalf("combined plan requires %#x", got)
	}
}

func TestValidateVerdict(t *testing.T) {
	if err := ValidateVerdict(VerdictAllow, 0); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if err := ValidateVerdict(VerdictDeny, 0); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if err := ValidateVerdict(3, 2); err != nil {
		t.Fatalf("route 1: %v", err)
	}
	if err := ValidateVerdict(4, 2); err == nil {
		t.Fatal("out-of-range route accepted")
	}
	if err := ValidateVerdict(2, 0); err == nil {
		t.Fatal("route without pools accepted")
	}
	if err := ValidateVerdict(-1, 0); err == nil {
		t.Fatal("negative verdict accepted")
	}
}

func TestPhasePlan(t *testing.T) {
	p := InspectRequestHeaders | InspectResponseHeaders
	if !p.Has(PhaseRequestHeaders) || !p.Has(PhaseResponseHeaders) {
		t.Fatal("plan misses a declared phase")
	}
	if p.Has(PhaseRequestBody) || p.Has(PhaseOpaqueStream) {
		t.Fatal("plan reports an undeclared phase")
	}
}
