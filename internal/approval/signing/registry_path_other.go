//go:build !darwin

package signing

// DefaultRegistryPath is the root-owned signers registry. The directory and the
// file must both be root-owned so the orch user cannot modify or replace it.
const DefaultRegistryPath = "/etc/aigate/signers.json"
