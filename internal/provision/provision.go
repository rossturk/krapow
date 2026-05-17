// Package provision renders the cloud-init / PowerShell scripts that bootstrap
// a fresh runner inside a newly-launched VM.
package provision

import (
	"bytes"
	_ "embed"
	"text/template"
)

//go:embed templates/linux-cloudinit.yaml
var linuxTpl string

//go:embed templates/windows-provision.ps1
var windowsTpl string

//go:embed templates/mac-provision.sh
var macTpl string

//go:embed templates/linuxarm-provision.sh
var linuxARMTpl string

type Vars struct {
	RepoURL  string
	RegToken string
	Name     string
	Labels   string
}

func LinuxCloudInit(v Vars) (string, error)    { return render(linuxTpl, v) }
func WindowsPS1(v Vars) (string, error)        { return render(windowsTpl, v) }
func MacProvision(v Vars) (string, error)      { return render(macTpl, v) }
func LinuxARMProvision(v Vars) (string, error) { return render(linuxARMTpl, v) }

func render(tpl string, v Vars) (string, error) {
	t, err := template.New("").Parse(tpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}
