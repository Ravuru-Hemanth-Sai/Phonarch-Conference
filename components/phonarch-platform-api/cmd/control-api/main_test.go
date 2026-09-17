package main

import "testing"

func TestNormalizeDialNumber(t *testing.T) {
	tests := []struct {
		name   string
		phone  string
		region string
		want   string
	}{
		{name: "room default india", phone: "098765 43210", region: "+91", want: "+919876543210"},
		{name: "international remains international", phone: "+44 20 7946 0958", region: "+91", want: "+442079460958"},
		{name: "invalid has no digits", phone: "---", region: "+91", want: ""},
		{name: "unknown region falls back to india", phone: "5550100", region: "+999", want: "+915550100"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeDialNumber(test.phone, test.region); got != test.want {
				t.Fatalf("normalizeDialNumber(%q, %q) = %q, want %q", test.phone, test.region, got, test.want)
			}
		})
	}
}

func TestNormalizeDialRegion(t *testing.T) {
	if got := normalizeDialRegion("+44"); got != "+44" {
		t.Fatalf("supported region normalized to %q", got)
	}
	if got := normalizeDialRegion("+999"); got != "+91" {
		t.Fatalf("unsupported region normalized to %q", got)
	}
}
