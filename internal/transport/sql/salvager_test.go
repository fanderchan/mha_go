package sql

import "testing"

func TestDonorContainsRequiredGTIDAllowsSuperset(t *testing.T) {
	got, err := donorContainsRequiredGTID(
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-100",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:40-45",
	)
	if err != nil {
		t.Fatalf("donorContainsRequiredGTID: %v", err)
	}
	if !got {
		t.Fatal("expected donor superset to contain the required GTID subset")
	}
}

func TestDonorContainsRequiredGTIDRejectsMissingInterval(t *testing.T) {
	got, err := donorContainsRequiredGTID(
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-39",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:40-45",
	)
	if err != nil {
		t.Fatalf("donorContainsRequiredGTID: %v", err)
	}
	if got {
		t.Fatal("expected donor missing the interval to be rejected")
	}
}
