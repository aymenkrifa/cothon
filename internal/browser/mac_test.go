package browser

import (
	"errors"
	"slices"
	"testing"
)

func TestMacArgv(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "mybrowser" {
			return "/usr/local/bin/mybrowser", nil
		}
		return "", errors.New("not found")
	}
	u := "http://127.0.0.1:1/x"
	for cmd, want := range map[string][]string{
		"":                           {"open", u},
		"firefox":                    {"open", "-a", "Firefox", u},
		"google-chrome --new-window": {"open", "-a", "Google Chrome", u, "--args", "--new-window"},
		"Arc":                        {"open", "-a", "Arc", u},
		"mybrowser --flag":           {"mybrowser", "--flag", u},
	} {
		if got := macArgv(cmd, u, look); !slices.Equal(got, want) {
			t.Errorf("macArgv(%q) = %q, want %q", cmd, got, want)
		}
	}
}
