package main

import "testing"

func TestParseResumeFlag(t *testing.T) {
	id, rest, err := parseResumeFlag([]string{"--resume", "20260102-150405", "do", "stuff"})
	if err != nil || id != "20260102-150405" || len(rest) != 2 || rest[0] != "do" {
		t.Fatalf("space form: id=%q rest=%v err=%v", id, rest, err)
	}
	id, rest, err = parseResumeFlag([]string{"--resume=abc-1", "task"})
	if err != nil || id != "abc-1" || len(rest) != 1 || rest[0] != "task" {
		t.Fatalf("equals form: id=%q rest=%v err=%v", id, rest, err)
	}
	id, rest, err = parseResumeFlag([]string{"plain"})
	if err != nil || id != "" || len(rest) != 1 {
		t.Fatalf("no flag: id=%q rest=%v err=%v", id, rest, err)
	}
}

func TestParseResumeFlagRejectsBadIDs(t *testing.T) {
	for _, bad := range []string{"../escape", "a/b", ".hidden", ""} {
		if _, _, err := parseResumeFlag([]string{"--resume=" + bad}); err == nil {
			t.Fatalf("id %q must be rejected", bad)
		}
	}
	// "--resume" with a missing value must error too.
	if _, _, err := parseResumeFlag([]string{"--resume"}); err == nil {
		t.Fatal("missing value must be rejected")
	}
}
