package main

import "testing"

func TestParseFlags(t *testing.T) {
	id, flags, rest, err := parseFlags([]string{"--resume", "20260102-150405", "do", "stuff"})
	if err != nil || id != "20260102-150405" || len(rest) != 2 || rest[0] != "do" || flags["router"] {
		t.Fatalf("space form: id=%q rest=%v router=%v err=%v", id, rest, flags["router"], err)
	}
	id, flags, rest, err = parseFlags([]string{"--router", "--resume=abc-1", "task"})
	if err != nil || id != "abc-1" || len(rest) != 1 || !flags["router"] {
		t.Fatalf("router+equals: id=%q rest=%v router=%v err=%v", id, rest, flags["router"], err)
	}
	id, flags, rest, err = parseFlags([]string{"plain"})
	if err != nil || id != "" || len(rest) != 1 || flags["router"] {
		t.Fatalf("no flag: id=%q rest=%v", id, rest)
	}
}

func TestParseFlagsRejectsBadIDs(t *testing.T) {
	for _, bad := range []string{"../escape", "a/b", ".hidden", ""} {
		if _, _, _, err := parseFlags([]string{"--resume=" + bad}); err == nil {
			t.Fatalf("id %q must be rejected", bad)
		}
	}
	if _, _, _, err := parseFlags([]string{"--resume"}); err == nil {
		t.Fatal("missing value must be rejected")
	}
}
