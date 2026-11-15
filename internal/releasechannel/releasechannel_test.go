package releasechannel

import "testing"

func some(name string) *string { return &name }

func eqParsed(t *testing.T, name *string, want Channel) {
	t.Helper()
	got, err := ParseChannel(name)
	if err != nil || got != want {
		t.Fatalf("ParseChannel = (%v, %v), want (%v, nil)", got, err, want)
	}
}

func eqRefused(t *testing.T, name *string) {
	t.Helper()
	if _, err := ParseChannel(name); err != (InvalidChannel{}) {
		t.Fatalf("ParseChannel(%q) err = %v, want InvalidChannel", *name, err)
	}
}

func TestNothingSetIsALocalBuild(t *testing.T) {
	eqParsed(t, nil, Local)
}

func TestTheTwoDistributedChannelsParse(t *testing.T) {
	eqParsed(t, some("dev"), Dev)
	eqParsed(t, some("public"), Public)
}

func TestLocalCannotBeAskedForByName(t *testing.T) {
	eqRefused(t, some("local"))
}

func TestAnythingElseIsRefused(t *testing.T) {
	eqRefused(t, some("stable"))
	eqRefused(t, some(""))
	eqRefused(t, some("Dev"))
}

func TestOnlyLocalStaysQuiet(t *testing.T) {
	if Local.PollsForUpdates() {
		t.Fatal("local polls")
	}
	if !Dev.PollsForUpdates() {
		t.Fatal("dev does not poll")
	}
	if !Public.PollsForUpdates() {
		t.Fatal("public does not poll")
	}
}
