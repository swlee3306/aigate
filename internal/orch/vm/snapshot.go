package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// OpenStackCLI is the real Cloud: it runs `<Prefix> <verb...>` where Prefix is
// by default `ssh openstack-host openstack --os-cloud exporter-internal`.
// Every command is checked against a strict allow-list of verbs, limited to
// the test VM's own resources by name, before it runs. The only delete verbs
// besides `server delete` are `volume delete <id>` and `volume attachment
// delete <id>`, and they run only inside Prune (see prune.go), only for ids
// that Prune re-read and verified a moment before, and only when
// PruneRestoreVolumes is set (default off).
//
// See the package doc for the open privilege question this raises.
type OpenStackCLI struct {
	Prefix []string
	// VM is the server name (default "test-vm"); the only server touched.
	VM string
	// Snapshot defaults to VM+"-clean". Flavor, Network, KeyName and FixedIP
	// default to the values infra/snapshot.sh uses for test-vm.
	Snapshot, Flavor, Network, KeyName, FixedIP string
	// Probe is one ssh attempt to the VM; WaitSSH polls it. Required for WaitSSH.
	Probe        func(ctx context.Context) error
	PollInterval time.Duration // default 2s
	// Run executes argv (prefix included); nil means exec. Tests replace it.
	Run func(ctx context.Context, argv []string) ([]byte, error)
	// PruneRestoreVolumes enables Prune (default false: Prune does nothing).
	// MaxPrune caps the volumes one Prune call tries to delete (default 5).
	PruneRestoreVolumes bool
	MaxPrune            int

	mu       sync.Mutex      // guards the two sets below
	okDelete map[string]bool // volume ids Prune verified; only these may be deleted
	okAttach map[string]bool // attachment ids Prune found stale; only these may be deleted
}

var (
	argRE  = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.,=:-]*$`)
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sizeRE = regexp.MustCompile(`^[0-9]{1,4}$`)
	// restoreRE matches test-vm-root-YYYYMMDD-HHMMSS-xxxx (the legacy
	// test-vm-root-HHMMSS is still recognised for listing and pruning).
	restoreRE = func(vm string) *regexp.Regexp {
		return regexp.MustCompile(`^` + regexp.QuoteMeta(vm) + `-root-([0-9]{8}-)?[0-9]{6}(-[0-9a-f]{4})?$`)
	}
)

func (o *OpenStackCLI) vm() string {
	if o.VM == "" {
		return "test-vm"
	}
	return o.VM
}
func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func (o *OpenStackCLI) snapshot() string { return def(o.Snapshot, o.vm()+"-clean") }
func (o *OpenStackCLI) flavor() string   { return def(o.Flavor, "aigate.test") }
func (o *OpenStackCLI) network() string  { return def(o.Network, "aigate-net") }
func (o *OpenStackCLI) keyName() string  { return def(o.KeyName, "aigate-key") }
func (o *OpenStackCLI) fixedIP() string  { return def(o.FixedIP, "10.70.0.13") }

func (o *OpenStackCLI) prefix() []string {
	if len(o.Prefix) > 0 {
		return o.Prefix
	}
	return []string{"ssh", "-o", "BatchMode=yes", "openstack-host", "openstack", "--os-cloud", "exporter-internal"}
}

// allowed reports whether args (the openstack verb and its arguments, without
// the prefix) is one of the few commands Restore needs, on this VM's resources.
func (o *OpenStackCLI) allowed(args []string) error {
	for _, a := range args {
		if !argRE.MatchString(a) {
			return fmt.Errorf("argument %q is not allowed", a)
		}
	}
	vm := o.vm()
	rv := restoreRE(vm)
	is := func(want ...string) bool {
		if len(args) != len(want) {
			return false
		}
		for i, w := range want {
			if w != "*" && args[i] != w {
				return false
			}
		}
		return true
	}
	switch {
	case is("server", "show", vm, "-f", "value", "-c", "id"):
	case is("server", "show", vm, "-f", "value", "-c", "status"):
	case is("server", "delete", "--wait", vm):
	case is("volume", "snapshot", "show", o.snapshot(), "-f", "value", "-c", "size"):
	case is("volume", "create", "--snapshot", o.snapshot(), "--size", "*", "*") && sizeRE.MatchString(args[5]) && rv.MatchString(args[6]):
	case is("volume", "show", "*", "-f", "value", "-c", "status") && rv.MatchString(args[2]):
	case is("network", "show", o.network(), "-f", "value", "-c", "id"):
	case is("server", "create", "--flavor", o.flavor(), "--volume", "*", "--nic", "*", "--key-name", o.keyName(), "--wait", vm, "-f", "value", "-c", "status") &&
		rv.MatchString(args[5]) && nicRE(o.fixedIP()).MatchString(args[7]):
	case is("volume", "list", "-f", "value", "-c", "ID", "-c", "Name"):
	// read-only verbs for Prune
	case is("server", "show", vm, "-f", "json", "-c", "volumes_attached"):
	case is("server", "list", "--all-projects", "-f", "value", "-c", "ID"):
	case is("volume", "snapshot", "list", "--all-projects", "-f", "value", "-c", "ID"):
	case is("volume", "snapshot", "show", "*", "-f", "value", "-c", "volume_id") && uuidRE.MatchString(args[3]):
	case is("volume", "show", "*", "-f", "json", "-c", "name", "-c", "status", "-c", "attachments") && uuidRE.MatchString(args[2]):
	// the only two deletes: by UUID, and only what Prune just verified
	case is("--os-volume-api-version", "3.44", "volume", "attachment", "delete", "*") && uuidRE.MatchString(args[5]) && o.permitted(o.okAttach, args[5]):
	case is("volume", "delete", "*") && uuidRE.MatchString(args[2]) && o.permitted(o.okDelete, args[2]):
	default:
		return errors.New("command is not in the allow-list")
	}
	return nil
}

func (o *OpenStackCLI) permitted(set map[string]bool, id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return set[id]
}

func nicRE(ip string) *regexp.Regexp {
	return regexp.MustCompile(`^net-id=[A-Za-z0-9-]{8,64},v4-fixed-ip=` + regexp.QuoteMeta(ip) + `$`)
}

func (o *OpenStackCLI) run(ctx context.Context, args ...string) (string, error) {
	if err := o.allowed(args); err != nil {
		return "", fmt.Errorf("openstack %s: refused: %w", strings.Join(args, " "), err)
	}
	argv := append(append([]string{}, o.prefix()...), args...)
	var out []byte
	var err error
	if o.Run != nil {
		out, err = o.Run(ctx, argv)
	} else {
		out, err = execRun(ctx, argv)
	}
	if err != nil {
		return "", fmt.Errorf("openstack %s: %w", strings.Join(args[:min(len(args), 3)], " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func execRun(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 2 * time.Second
	var so, se limitedBuf
	so.max, se.max = 64<<10, 1024
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(bytes.TrimSpace(se.b))))
	}
	return so.b, nil
}

func (o *OpenStackCLI) waitStatus(ctx context.Context, want string, args ...string) error {
	iv := o.PollInterval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	for {
		s, err := o.run(ctx, args...)
		if err == nil {
			if s == want {
				return nil
			}
			if s == "ERROR" {
				return fmt.Errorf("%s is ERROR", args[0])
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for %s -> %s: %w", args[0], want, ctx.Err())
		case <-time.After(iv):
		}
	}
}

// DeleteServer deletes the VM's server and keeps its volume.
func (o *OpenStackCLI) DeleteServer(ctx context.Context, server string) error {
	if server != o.vm() {
		return fmt.Errorf("refused: server %q is not %q", server, o.vm())
	}
	if _, err := o.run(ctx, "server", "show", server, "-f", "value", "-c", "id"); err != nil {
		return nil // no such server (a failing show is also what a missing one does); create fails loudly if it was something else
	}
	_, err := o.run(ctx, "server", "delete", "--wait", server)
	return err
}

// CreateVolumeFromSnapshot creates a new volume of the snapshot's size.
func (o *OpenStackCLI) CreateVolumeFromSnapshot(ctx context.Context, snapshot, volume string) error {
	if snapshot != o.snapshot() {
		return fmt.Errorf("refused: snapshot %q is not %q", snapshot, o.snapshot())
	}
	size, err := o.run(ctx, "volume", "snapshot", "show", snapshot, "-f", "value", "-c", "size")
	if err != nil {
		return err
	}
	if _, err := o.run(ctx, "volume", "create", "--snapshot", snapshot, "--size", size, volume); err != nil {
		return err
	}
	return o.waitStatus(ctx, "available", "volume", "show", volume, "-f", "value", "-c", "status")
}

// CreateServer boots the VM from volume on aigate-net with its fixed address.
func (o *OpenStackCLI) CreateServer(ctx context.Context, server, volume string) error {
	if server != o.vm() {
		return fmt.Errorf("refused: server %q is not %q", server, o.vm())
	}
	net, err := o.run(ctx, "network", "show", o.network(), "-f", "value", "-c", "id")
	if err != nil {
		return err
	}
	// no user-data: the snapshot already holds users, rules and config
	_, err = o.run(ctx, "server", "create", "--flavor", o.flavor(), "--volume", volume,
		"--nic", "net-id="+net+",v4-fixed-ip="+o.fixedIP(), "--key-name", o.keyName(), "--wait", server,
		"-f", "value", "-c", "status")
	return err
}

// WaitSSH polls Probe until it succeeds or ctx ends.
func (o *OpenStackCLI) WaitSSH(ctx context.Context, server string) error {
	if o.Probe == nil {
		return errors.New("OpenStackCLI.Probe is not set")
	}
	iv := o.PollInterval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	for {
		if err := o.Probe(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ssh to %s did not come back: %w", server, ctx.Err())
		case <-time.After(iv):
		}
	}
}

// ListRestoreVolumes lists volumes named like this VM's restore volumes plus
// the golden volume, read-only, for manual pruning. It never deletes.
func (o *OpenStackCLI) ListRestoreVolumes(ctx context.Context) ([]string, error) {
	out, err := o.run(ctx, "volume", "list", "-f", "value", "-c", "ID", "-c", "Name")
	if err != nil {
		return nil, err
	}
	rv := restoreRE(o.vm())
	var vols []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && (rv.MatchString(f[1]) || f[1] == o.vm()+"-root") {
			vols = append(vols, f[1]+" "+f[0])
		}
	}
	return vols, nil
}
