// Package incus is a thin wrapper around the `incus` CLI.
package incus

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// StreamOut and StreamErr are where runStream writes subprocess output. Default
// to os.Stdout/Stderr; cmd-level code overrides them (e.g. to a TUI viewport).
var (
	StreamOut io.Writer = os.Stdout
	StreamErr io.Writer = os.Stderr
)

func run(args ...string) (string, error) {
	cmd := exec.Command("incus", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("incus %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func runStream(args ...string) error {
	cmd := exec.Command("incus", args...)
	cmd.Stdout = StreamOut
	cmd.Stderr = StreamErr
	return cmd.Run()
}

// LaunchVM creates and starts a VM from `image`, applying the given config
// keys and device overrides. deviceKV uses "device.key=value" semantics:
//
//	deviceKV: map[string]string{"root.size": "40GiB"}  →  -d root,size=40GiB
func LaunchVM(image, name string, configKV, deviceKV map[string]string) error {
	args := []string{"launch", "--quiet", image, name, "--vm"}
	for k, v := range configKV {
		args = append(args, "-c", k+"="+v)
	}
	for k, v := range deviceKV {
		// "root.size" → "root,size=value"
		dot := strings.IndexByte(k, '.')
		if dot <= 0 {
			return fmt.Errorf("device override key %q must be <device>.<key>", k)
		}
		args = append(args, "-d", k[:dot]+","+k[dot+1:]+"="+v)
	}
	return runStream(args...)
}

// ImageExists checks the local image store for `alias`. Accepts both bare
// alias and "local:alias" forms (the latter is what `incus launch` wants but
// `incus image list` rejects as a filter).
func ImageExists(alias string) (bool, error) {
	a := strings.TrimPrefix(alias, "local:")
	out, err := run("image", "list", a, "--format", "csv", "-c", "l")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// State returns the instance status (e.g. "running", "stopped"), or "absent".
func State(name string) string {
	out, err := run("list", name, "--format", "csv", "-c", "ns")
	if err != nil {
		return "absent"
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, ",", 2)
		if len(parts) == 2 && parts[0] == name {
			return strings.ToLower(parts[1])
		}
	}
	return "absent"
}

func Start(name string) error  { return runStream("start", name) }
func Stop(name string) error   { return runStream("stop", name) }
func Delete(name string) error { return runStream("delete", "--force", name) }

// Instances returns the names of every instance the incus daemon knows about.
// Used by `krapow clean` to enumerate candidates for orphan removal.
func Instances() ([]string, error) {
	out, err := run("list", "--format", "csv", "-c", "n")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

func Exec(name string, args ...string) error {
	return runStream(append([]string{"exec", name, "--"}, args...)...)
}

// AddImageAlias adds a second alias `newAlias` pointing at the same fingerprint
// as `existingAlias`. Useful for renaming an imported image without recopying it.
func AddImageAlias(existingAlias, newAlias string) error {
	out, err := run("image", "list", existingAlias, "--format", "csv", "-c", "f")
	if err != nil {
		return err
	}
	fp := strings.TrimSpace(out)
	if fp == "" {
		return fmt.Errorf("no image found for alias %q", existingAlias)
	}
	// Tolerate the case where the alias already exists.
	if _, err := run("image", "alias", "create", newAlias, fp); err != nil {
		if strings.Contains(err.Error(), "already") {
			return nil
		}
		return err
	}
	return nil
}

// AddDevice attaches a device of `devType` with the given key=value config to
// an existing instance.
//
//	AddDevice("win-base", "iso-agent", "disk", map[string]string{"source": "agent:config"})
func AddDevice(instance, name, devType string, kv map[string]string) error {
	args := []string{"config", "device", "add", instance, name, devType}
	for k, v := range kv {
		args = append(args, k+"="+v)
	}
	return runStream(args...)
}

// SetConfig sets an instance config key (e.g. "image.os").
func SetConfig(instance, key, value string) error {
	return runStream("config", "set", instance, key, value)
}

// ExecCapture runs `incus exec` and returns stdout (used for readiness probes).
func ExecCapture(name string, args ...string) (string, error) {
	return run(append([]string{"exec", name, "--"}, args...)...)
}
