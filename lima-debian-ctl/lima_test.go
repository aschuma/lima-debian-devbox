package main

import "testing"

func TestLastLimaMessage(t *testing.T) {
	cases := map[string]string{
		"": "",
		"time=\"x\" level=fatal msg=\"cannot edit a running instance\"\n": "cannot edit a running instance",
		"line one\nplain final line\n\n":                                  "plain final line",
	}
	for in, want := range cases {
		if got := lastLimaMessage(in); got != want {
			t.Errorf("lastLimaMessage(%q) = %q, want %q", in, got, want)
		}
	}
}
