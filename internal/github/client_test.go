package github

import (
	"testing"
)

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"owner/repo", "owner", "repo", false},
		{"acme/project-a", "acme", "project-a", false},
		{"noslash", "", "", true},
		{"", "", "", true},
		{"/repo", "", "", true},
		{"owner/", "", "", true},
	}

	for _, tc := range cases {
		owner, repo, err := SplitRepo(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SplitRepo(%q) expected error, got nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitRepo(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if owner != tc.wantOwner {
			t.Errorf("SplitRepo(%q) owner = %q, want %q", tc.input, owner, tc.wantOwner)
		}
		if repo != tc.wantRepo {
			t.Errorf("SplitRepo(%q) repo = %q, want %q", tc.input, repo, tc.wantRepo)
		}
	}
}
