package service

import "testing"

func TestMatches(t *testing.T) {
	cases := []struct {
		pattern string
		event   string
		want    bool
	}{
		{"*", "file.uploaded", true},
		{"*", "", false},
		{"file.uploaded", "file.uploaded", true},
		{"file.uploaded", "file.deleted", false},
		{"file.*", "file.uploaded", true},
		{"file.*", "file.", false},
		{"file.*", "files.uploaded", false},
		{"file.*", "file", false},
		{" user.* ", "user.registered", true},
	}
	for _, tc := range cases {
		if got := Match(tc.pattern, tc.event); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.event, got, tc.want)
		}
	}

	if !Matches([]string{"user.*", "file.uploaded"}, "file.uploaded") {
		t.Error("Matches should accept any matching pattern")
	}
	if Matches(nil, "file.uploaded") {
		t.Error("empty patterns must not match")
	}
}

func TestValidType(t *testing.T) {
	valid := []string{"*", "file.*", "file.uploaded", "file_v2.uploaded", "a-b.c"}
	for _, value := range valid {
		if !ValidType(value) {
			t.Errorf("ValidType(%q) = false, want true", value)
		}
	}
	invalid := []string{"", ".*", "File.uploaded", "file uploaded", "file/*", "file.*.uploaded"}
	for _, value := range invalid {
		if ValidType(value) {
			t.Errorf("ValidType(%q) = true, want false", value)
		}
	}
}

func TestNormalizePatterns(t *testing.T) {
	got := NormalizePatterns([]string{" file.* ", "file.uploaded", "file.*", "", "Bad", "user.registered"})
	want := []string{"file.*", "file.uploaded", "user.registered"}
	if len(got) != len(want) {
		t.Fatalf("NormalizePatterns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizePatterns = %v, want %v", got, want)
		}
	}
}
