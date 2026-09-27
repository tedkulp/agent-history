package release

import "testing"

func TestCheckFloorAcceptsTagAtOrAboveFloor(t *testing.T) {
	for _, tag := range []string{"v0.4.0", "v0.4.1", "v1.0.0"} {
		if err := CheckFloor("0.4.0", tag); err != nil {
			t.Errorf("CheckFloor(0.4.0, %s) = %v, want nil", tag, err)
		}
	}
}

func TestCheckFloorRejectsTagBelowFloor(t *testing.T) {
	err := CheckFloor("0.5.0", "v0.4.9")
	if err == nil {
		t.Fatal("CheckFloor(0.5.0, v0.4.9) = nil, want an error")
	}
	if got, want := err.Error(), "minimum Collector version 0.5.0 is above the release version 0.4.9"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// An rc cycle that raises the floor works: 0.5.0-rc.1 counts as 0.5.0.
func TestCheckFloorDropsPreReleaseSuffix(t *testing.T) {
	if err := CheckFloor("0.5.0", "v0.5.0-rc.1"); err != nil {
		t.Errorf("CheckFloor(0.5.0, v0.5.0-rc.1) = %v, want nil", err)
	}
	if err := CheckFloor("0.5.0", "v0.4.0-rc.1"); err == nil {
		t.Error("CheckFloor(0.5.0, v0.4.0-rc.1) = nil, want an error")
	}
}

func TestCheckFloorRejectsMalformedTag(t *testing.T) {
	for _, tag := range []string{"0.4.0", "v0.4", "vnext", ""} {
		if err := CheckFloor("0.1.0", tag); err == nil {
			t.Errorf("CheckFloor(0.1.0, %q) = nil, want an error", tag)
		}
	}
}
