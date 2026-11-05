package hosts

import (
	"os/exec"
	"strings"
	"testing"
)

func probeOut(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func TestPlanSetup(t *testing.T) {
	ubuntu := []string{"id=ubuntu", "version=24.04", "codename=noble", "name=Ubuntu 24.04.3 LTS", "arch=x86_64", "uid=1000", "user=hlm", "systemd=yes"}
	for _, tt := range []struct {
		name     string
		lines    []string
		distro   string
		canSetUp bool
		ready    bool
		install  bool   // the script installs Docker
		want     string // in the script
		reason   string // in the reason
	}{
		{"ubuntu with sudo", append(ubuntu, "sudo=yes"), "ubuntu", true, false, true, "URIs: https://download.docker.com/linux/ubuntu", ""},
		{"ubuntu without sudo", ubuntu, "ubuntu", false, false, true, "usermod -aG docker 'hlm'", "passwordless sudo"},
		{"ready", append(ubuntu, "docker=yes", "socket=yes", "server=29.0.1", "group=yes"), "", false, true, false, "", ""},
		{"installed, not in the group", append(ubuntu, "docker=yes", "socket=yes", "sudo=yes"), "", true, false, false, "usermod -aG docker 'hlm'", ""},
		{"debian as root", []string{"id=debian", "codename=trixie", "arch=aarch64", "uid=0", "user=root", "systemd=yes"}, "debian", true, false, true, "https://download.docker.com/linux/debian/gpg", ""},
		{"64-bit pi os", []string{"id=raspbian", "arch=aarch64", "uid=0", "user=root", "systemd=yes"}, "debian", true, false, true, "linux/debian", ""},
		{"32-bit pi os", []string{"id=raspbian", "arch=armv7l", "uid=0", "user=root", "systemd=yes"}, "raspbian", true, false, true, "docker.list", ""},
		{"pi zero", []string{"id=raspbian", "arch=armv6l", "uid=0", "user=root"}, "", false, false, false, "", "ARMv6"},
		{"fedora", []string{"id=fedora", "uid=0", "user=root", "systemd=yes"}, "fedora", true, false, true, "addrepo --from-repofile", ""},
		{"rhel", []string{"id=rhel", "uid=0", "user=root", "systemd=yes"}, "rhel", true, false, true, "linux/rhel/docker-ce.repo", ""},
		{"centos", []string{"id=centos", "uid=0", "user=root", "systemd=yes"}, "centos", true, false, true, "linux/centos/docker-ce.repo", ""},
		{"unraid-like", []string{"id=slackware", "name=Slackware", "uid=0", "user=root"}, "", false, false, false, "", "doesn't publish packages for Slackware"},
		{"installed without systemd", []string{"id=alpine", "docker=yes", "uid=1000", "user=hlm"}, "", false, false, false, "", "systemd"},
	} {
		in := parseProbe(probeOut(tt.lines...))
		if in.Distro != tt.distro || in.CanSetUp != tt.canSetUp || in.Ready != tt.ready {
			t.Errorf("%s: distro %q, can set up %v, ready %v", tt.name, in.Distro, in.CanSetUp, in.Ready)
		}
		if installs := strings.Contains(in.Script, "Install Docker."); installs != tt.install {
			t.Errorf("%s: script installs = %v", tt.name, installs)
		}
		if !strings.Contains(in.Script, tt.want) || !strings.Contains(in.Reason, tt.reason) {
			t.Errorf("%s: script/reason missing %q/%q:\n%s\n%s", tt.name, tt.want, tt.reason, in.Script, in.Reason)
		}
		if strings.Contains(in.Script, "sudo ") {
			t.Errorf("%s: the script runs as root and shouldn't use sudo", tt.name)
		}
		if in.Script != "" {
			// Every script is valid shell.
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(in.Script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: sh -n: %v %s", tt.name, err, out)
			}
		}
	}
}

func TestSetupScriptQuotesUser(t *testing.T) {
	s := setupScript("ubuntu", "o'brien", false)
	if !strings.Contains(s, `usermod -aG docker 'o'"'"'brien'`) {
		t.Errorf("user not quoted:\n%s", s)
	}
	if s := setupScript("ubuntu", "root", true); strings.Contains(s, "usermod") || strings.Contains(s, "apt-get") {
		t.Errorf("root, already installed:\n%s", s)
	}
}
