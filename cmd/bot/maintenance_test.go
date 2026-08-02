package main

import "testing"

func TestValidCanonicalIdentity(t *testing.T) {
	valid := []string{
		"union:abc",
		"user:abc",
		"member:group:member",
	}
	for _, value := range valid {
		if !validCanonicalIdentity(value) {
			t.Fatalf("expected %q to be valid", value)
		}
	}
	invalid := []string{"", "abc", "union:", "user:", "member:group", "member::member"}
	for _, value := range invalid {
		if validCanonicalIdentity(value) {
			t.Fatalf("expected %q to be invalid", value)
		}
	}
}
