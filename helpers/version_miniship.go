package helpers

// The upstream tag this branch sits on, on the miniship line.
//
// A `rest` image is built by the public monorepo with
// `go build -trimpath -ldflags="-s -w"` and no `-X`, so `Version` is empty and
// what `prestd version` prints is the fallback below. Upstream leaves that
// fallback at 2.0.0 on a v2.4.2 tree, which tells someone holding only an
// image nothing about which upstream release it carries.
//
// **It is set here, in a file of miniship's own, rather than edited in
// prest.go.** Upstream has moved that literal repeatedly for its own version
// bumps, so a patch sitting on the same line conflicts on every rebase; a new
// file touches no upstream line and conflicts with nothing. It is the same
// reason patch 4 leaves the unrouted handlers in the tree.
//
// `init` runs after every package-level variable is initialised and before
// anything outside this package can read one, so the value upstream assigns is
// never observed. MINISHIP.md states the tag and
// cmd/version_miniship_test.go holds this string to it, so a rebase that moves
// one and not the other is red.
func init() {
	PrestVersionNumber = "2.4.2+miniship"
}
