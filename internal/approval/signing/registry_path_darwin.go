//go:build darwin

package signing

// DefaultRegistryPath is the root-owned signers registry. The directory and the
// file must both be root-owned so the orch user cannot modify or replace it.
//
// On macOS /etc is a symlink to private/etc, and LoadRegistry refuses a symlink
// as the nearest existing ancestor of a missing registry, so the default names
// the real directory. It is the same location as /etc/aigate on Linux.
const DefaultRegistryPath = "/private/etc/aigate/signers.json"
