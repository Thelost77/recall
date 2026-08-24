package project

import "testing"

func TestNormalizeRemoteCommonForms(t *testing.T) {
	forms := []string{
		"git@github.com:owner/repository.git",
		"https://github.com/owner/repository.git",
		"ssh://git@github.com/owner/repository.git",
	}
	for _, form := range forms {
		if got := NormalizeRemote(form); got != "github.com/owner/repository" {
			t.Fatalf("NormalizeRemote(%q) = %q", form, got)
		}
	}
}
