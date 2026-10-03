package sdk

// THE LINUX HALF OF messageFragmentPartSizeCopyRulings, for a copy of the part size only a linux build
// can see.
//
// unix.O_NONBLOCK is 0x800 on linux, which is 2048, the part size, and 4 on darwin and the BSDs. The
// gate's class is the VALUE, read off the type checker, so the flag is in it here and nowhere else. A
// Windows build cannot see it at all: the file is unix-only, and the syntax-tree pass over the files
// a build does not compile cannot evaluate an imported constant. An excuse in the portable table
// would be stale on every build but this one, so it lives here, and its twin holds the empty half.
var messageFragmentPartSizePlatformCopyRulings = map[string]string{
	"peer_client_key_pin_store_open_unix.go openBoundedPeerPinFile": "unix.O_NONBLOCK -- an open(2) flag bit, " +
		"0x800 on linux. Upstream's bounded peer key-pin store opens its file with it, since the merge of " +
		"urnetwork/sdk main (msgrepo ledger 277). A flag, not a byte count of any kind",
}
