package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// The Windows app refuses an over-long text BEFORE it is sent, against its own copy of the two
// limits, and that copy is static_assert'ed at compile time against the header's #defines. This is
// the link that copy cannot make: the header against the constants the sealer itself refuses by.
func TestTheHeadersTextLimitsAreTheSealersOwn(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	header, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "include", "urnetwork_message.h"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{
		"URNET_MESSAGE_MAX_TEXT_OCTETS":       urmessage.MaxTextOctets,
		"URNET_MESSAGE_MAX_REPLY_TEXT_OCTETS": urmessage.MaxReplyTextOctets,
	} {
		match := regexp.MustCompile(`(?m)^#define ` + name + ` (\d+)\s*$`).FindSubmatch(header)
		if match == nil {
			t.Fatalf("include/urnetwork_message.h defines no %s", name)
		}
		got, err := strconv.Atoi(string(match[1]))
		if err != nil || got != want {
			t.Fatalf("%s is %s in the header and the sealer refuses above %d", name, match[1], want)
		}
	}
}
