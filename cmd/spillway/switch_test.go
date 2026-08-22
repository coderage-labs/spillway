package main

import (
	"errors"
	"strings"
	"testing"
)

// "auto" is the word a person types. --auto is the flag; off and none were
// already accepted; bare auto fell through to the default branch and became
// an account name, so it failed with `no account named "auto"` — a confusing
// way to be told about a flag.
func TestParseSwitchArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want switchArgs
		err  bool
	}{
		// "auto" is the word a person types; --auto is the flag. Bare auto
		// used to fall through to the default branch and become an account
		// name, failing with `no account named "auto"`.
		{[]string{"auto"}, switchArgs{auto: true}, false},
		{[]string{"--auto"}, switchArgs{auto: true}, false},
		{[]string{"off"}, switchArgs{auto: true}, false},
		{[]string{"none"}, switchArgs{auto: true}, false},
		{[]string{"work"}, switchArgs{name: "work"}, false},
		{[]string{"work", "--force"}, switchArgs{name: "work", force: true}, false},
		{[]string{"-f", "work"}, switchArgs{name: "work", force: true}, false},
		{nil, switchArgs{}, true},
		{[]string{"--nope"}, switchArgs{}, true},
	} {
		got, err := parseSwitchArgs(tc.args)
		if (err != nil) != tc.err {
			t.Errorf("%v: err = %v, want error = %v", tc.args, err, tc.err)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("%v: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

// The hint has to print after the error, because main prints the error and a
// hint written to stderr first landed above the thing it advises on.
func TestHintedErrorPutsAdviceAfterTheFailure(t *testing.T) {
	e := &hintedError{err: errors.New("pinning there would spend money (409)"), hint: "re-run with --force if you mean it"}
	msg := e.Error()
	if strings.Index(msg, "spend money") > strings.Index(msg, "--force") {
		t.Errorf("advice precedes the failure:\n%s", msg)
	}
	if !errors.Is(e, e.err) {
		t.Error("wrapped error is not unwrappable, so errors.Is on the cause fails")
	}
}
