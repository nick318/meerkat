package autoupdate

import "testing"

func ready(version string, sha *string) *Ready {
	return &Ready{
		Version: version,
		Sha:     sha,
	}
}

// Closing the toast silences that one build and nothing else.
func TestADismissalCoversOneBuild(t *testing.T) {
	one := ready("0.1.0", strPtr("2342a00"))

	if !Announces(one, nil) {
		t.Fatal("nothing dismissed yet")
	}
	if Announces(one, one) {
		t.Fatal("this is the one that was closed")
	}

	// The dev channel rarely bumps the version, so the commit is what
	// says a second update landed. It gets its own toast.
	next := ready("0.1.0", strPtr("beefcaf"))
	if !Announces(next, one) {
		t.Fatal("a new commit did not announce itself")
	}

	// And the public channel moves by version.
	if !Announces(ready("0.2.0", nil), ready("0.1.0", nil)) {
		t.Fatal("a new version did not announce itself")
	}
}
