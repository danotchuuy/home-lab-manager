package hosts

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// Checking a host reached over SSH for Docker, and installing Docker CE on
// it the way Docker's own install pages do (docs.docker.com/engine/install):
// Docker's apt or dnf repository, then docker-ce and its plugins, the
// service started on boot, and the SSH user in the docker group.

// Inspection is what hlm found on a host over SSH.
type Inspection struct {
	OS       string `json:"os"`       // PRETTY_NAME
	ID       string `json:"id"`       // ID from /etc/os-release
	Version  string `json:"version"`  // VERSION_ID
	Codename string `json:"codename"` // VERSION_CODENAME (UBUNTU_CODENAME on Ubuntu)
	Arch     string `json:"arch"`     // uname -m
	User     string `json:"user"`
	Root     bool   `json:"root"`
	// Sudo is set when the user can run sudo without a password.
	Sudo bool `json:"sudo"`
	// Docker is set when the docker command is installed; Running when
	// Docker's socket is there; DockerVersion (the daemon's version) when
	// the user can use it.
	Docker        bool   `json:"docker"`
	Running       bool   `json:"running"`
	DockerVersion string `json:"dockerVersion,omitempty"`
	InGroup       bool   `json:"inGroup"`
	Systemd       bool   `json:"systemd"`

	// Ready is set when hlm can use Docker there as it is.
	Ready bool `json:"ready"`
	// Distro is the Docker install page these steps follow ("ubuntu",
	// "debian", "raspbian", "fedora", "centos" or "rhel"); empty when Docker
	// doesn't publish packages for the host.
	Distro string `json:"distro,omitempty"`
	// Script is what setting Docker up runs there, as root.
	Script string `json:"script,omitempty"`
	// CanSetUp is set when hlm can run Script: the user is root or has
	// passwordless sudo. Otherwise Reason says why not.
	CanSetUp bool     `json:"canSetUp"`
	Reason   string   `json:"reason,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// probe reads the host's OS and what the user can do with Docker.
const probe = `. /etc/os-release 2>/dev/null
echo "id=$ID"
echo "version=$VERSION_ID"
echo "codename=${UBUNTU_CODENAME:-$VERSION_CODENAME}"
echo "name=$PRETTY_NAME"
echo "arch=$(uname -m)"
echo "uid=$(id -u)"
echo "user=$(id -un)"
command -v docker >/dev/null 2>&1 && echo "docker=yes"
[ -S /var/run/docker.sock ] && echo "socket=yes"
v=$(docker version --format '{{.Server.Version}}' 2>/dev/null) && [ -n "$v" ] && echo "server=$v"
id -nG 2>/dev/null | tr ' ' '\n' | grep -qx docker && echo "group=yes"
command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null && echo "sudo=yes"
command -v systemctl >/dev/null 2>&1 && echo "systemd=yes"
true`

// sshTarget is the SSH part of an input, for checking a host before it's
// saved (its data folder may not be filled in yet).
func sshTarget(in Input) (remote.Target, error) {
	if in.Kind != KindSSH || in.SSH == nil {
		return remote.Target{}, errors.New("only hosts reached over SSH can be checked and set up from hlm")
	}
	t := *in.SSH
	t.Host, t.User = strings.TrimSpace(t.Host), strings.TrimSpace(t.User)
	if t.Host == "" || t.User == "" {
		return remote.Target{}, errors.New("an SSH host and user are needed")
	}
	if t.Port == 0 {
		t.Port = 22
	}
	return t, nil
}

func (r *Registry) dialSSH(ctx context.Context, t remote.Target) (*remote.Conn, error) {
	if r.opts.SSHKey == nil {
		return nil, errors.New("hlm has no SSH key")
	}
	kp, ok, err := r.opts.SSHKey()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("hlm has no SSH key yet: make one first")
	}
	return remote.Dial(ctx, t, kp)
}

// Inspect signs in to a host over SSH and reports whether Docker is
// there, and how it would be set up if not. The host key must be trusted
// (or the *remote.HostKeyError says what to confirm).
func (r *Registry) Inspect(ctx context.Context, in Input) (Inspection, error) {
	t, err := sshTarget(in)
	if err != nil {
		return Inspection{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := r.dialSSH(ctx, t)
	if err != nil {
		return Inspection{}, err
	}
	defer c.Close()
	return inspect(ctx, c)
}

func inspect(ctx context.Context, c *remote.Conn) (Inspection, error) {
	res, err := c.Run(ctx, "sh -c "+remote.Quote(probe), nil)
	if err != nil {
		return Inspection{}, err
	}
	if res.ExitCode != 0 {
		return Inspection{}, fmt.Errorf("checking the host failed: %s", res.Output())
	}
	return parseProbe(res.Stdout), nil
}

func parseProbe(out string) Inspection {
	var in Inspection
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "id":
			in.ID = v
		case "version":
			in.Version = v
		case "codename":
			in.Codename = v
		case "name":
			in.OS = v
		case "arch":
			in.Arch = v
		case "uid":
			in.Root = v == "0"
		case "user":
			in.User = v
		case "docker":
			in.Docker = true
		case "socket":
			in.Running = true
		case "server":
			in.DockerVersion = v
		case "group":
			in.InGroup = true
		case "sudo":
			in.Sudo = true
		case "systemd":
			in.Systemd = true
		}
	}
	if in.OS == "" {
		in.OS = in.ID
	}
	in.Ready = in.DockerVersion != ""
	in.plan()
	return in
}

// The packages every install page installs.
const packages = "docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin"

// plan works out the setup steps for the host, from Docker's install page
// for its distribution.
func (in *Inspection) plan() {
	if in.Ready {
		return
	}
	if in.Docker {
		// Installed (however): it only needs starting, or the user adding
		// to the docker group.
		if !in.Systemd {
			in.Reason = "Docker is installed, but hlm can't use it, and the host doesn't use systemd: start Docker and add " + in.User + " to the docker group yourself."
			return
		}
		in.Script = setupScript("", in.User, true)
		in.privileges()
		return
	}
	switch {
	case in.ID == "ubuntu":
		in.Distro = "ubuntu"
	case in.ID == "debian":
		in.Distro = "debian"
	case in.ID == "raspbian" && (in.Arch == "aarch64" || in.Arch == "arm64"):
		// 64-bit Raspberry Pi OS follows Debian's page.
		in.Distro = "debian"
	case in.ID == "raspbian" && strings.HasPrefix(in.Arch, "armv6"):
		in.Reason = "Docker doesn't publish packages for ARMv6 (Raspberry Pi 1 and Zero)."
	case in.ID == "raspbian":
		in.Distro = "raspbian"
		in.Notes = append(in.Notes, "Docker Engine 28 is the last version for 32-bit Raspberry Pi OS; 64-bit Raspberry Pi OS keeps getting updates.")
	case in.ID == "fedora":
		in.Distro = "fedora"
	case in.ID == "centos":
		in.Distro = "centos"
	case in.ID == "rhel":
		in.Distro = "rhel"
	case in.ID == "":
		in.Reason = "hlm couldn't tell which Linux this is (no /etc/os-release)."
	default:
		in.Reason = fmt.Sprintf("Docker doesn't publish packages for %s; install Docker there yourself (docs.docker.com/engine/install), then check again.", in.OS)
	}
	if in.Distro == "" {
		return
	}
	in.Script = setupScript(in.Distro, in.User, false)
	in.privileges()
}

// privileges says whether hlm can run the script: as root, or with sudo
// that doesn't ask for a password.
func (in *Inspection) privileges() {
	if in.Root || in.Sudo {
		in.CanSetUp = true
		return
	}
	in.Reason = fmt.Sprintf("Setting Docker up needs root, and %s can't run sudo without a password: sign in as root, allow passwordless sudo for %s, or run the commands below yourself as root.", in.User, in.User)
}

// setupScript is what setting Docker up runs, as root: Docker's install
// steps for the distribution (without sudo, and without questions) when
// the docker command is missing, then the service started on boot and the
// user added to the docker group.
func setupScript(distro, user string, installed bool) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	if !installed {
		b.WriteString(installSteps(distro))
	}
	b.WriteString("\n# Start Docker now and on boot.\nsystemctl enable --now docker.service containerd.service\n")
	if user != "" && user != "root" {
		fmt.Fprintf(&b, "\n# Let %s use Docker without sudo.\ngetent group docker >/dev/null || groupadd docker\nusermod -aG docker %s\n", user, remote.Quote(user))
	}
	b.WriteString("\ndocker version --format 'Docker {{.Server.Version}} is running.'\n")
	return b.String()
}

func installSteps(distro string) string {
	switch distro {
	case "ubuntu", "debian":
		conflicts := "docker.io docker-compose docker-compose-v2 docker-doc docker-buildx podman-docker containerd runc"
		suite := `$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")`
		if distro == "debian" {
			conflicts = "docker.io docker-compose docker-doc docker-buildx podman-docker containerd runc"
			suite = `$(. /etc/os-release && echo "$VERSION_CODENAME")`
		}
		return `export DEBIAN_FRONTEND=noninteractive

# Remove unofficial packages that conflict with Docker's.
old=$(dpkg --get-selections ` + conflicts + ` 2>/dev/null | cut -f1)
[ -z "$old" ] || apt-get remove -y $old

# Add Docker's official GPG key.
apt-get update
apt-get install -y ca-certificates curl
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/` + distro + `/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

# Add the repository to Apt sources.
tee /etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/` + distro + `
Suites: ` + suite + `
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF
apt-get update

# Install Docker.
apt-get install -y ` + packages + `
`
	case "raspbian":
		return `export DEBIAN_FRONTEND=noninteractive

# Remove unofficial packages that conflict with Docker's.
for pkg in docker.io docker-doc docker-compose podman-docker containerd runc; do apt-get remove -y $pkg 2>/dev/null || true; done

# Add Docker's official GPG key.
apt-get update
apt-get install -y ca-certificates curl
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/raspbian/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

# Add the repository to Apt sources.
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/raspbian \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
  tee /etc/apt/sources.list.d/docker.list > /dev/null
apt-get update

# Install Docker.
apt-get install -y ` + packages + `
`
	default: // fedora, centos, rhel
		conflicts := "docker docker-client docker-client-latest docker-common docker-latest docker-latest-logrotate docker-logrotate docker-engine"
		var repo string
		switch distro {
		case "fedora":
			conflicts = "docker docker-client docker-client-latest docker-common docker-latest docker-latest-logrotate docker-logrotate docker-selinux docker-engine-selinux docker-engine"
			repo = "dnf config-manager addrepo --from-repofile https://download.docker.com/linux/fedora/docker-ce.repo"
		case "rhel":
			conflicts += " podman runc"
			fallthrough
		default:
			repo = "dnf -y install dnf-plugins-core\ndnf config-manager --add-repo https://download.docker.com/linux/" + distro + "/docker-ce.repo"
		}
		return `# Remove unofficial packages that conflict with Docker's.
dnf -y remove ` + conflicts + ` || true

# Set up Docker's repository.
` + repo + `

# Trust Docker's signing key only if it's the one Docker publishes
# (060A 61C5 1B55 8A7F 742B 77AA C52F EB6B 621E 9F35).
rpm --import https://download.docker.com/linux/` + distro + `/gpg
# (RPM 6 names keys by their fingerprint, older RPM by the last 8 digits.)
rpm -qa 'gpg-pubkey*' --qf '%{VERSION}\n' | grep -qix -e 060a61c51b558a7f742b77aac52feb6b621e9f35 -e 621e9f35 ||
  { echo "Docker's signing key isn't the expected one" >&2; exit 1; }

# Install Docker.
dnf -y install ` + packages + `
`
	}
}

// SetUpDocker runs the setup steps on a host over SSH, passing each line
// of output to onLine, and checks Docker answers the user afterwards (in a
// new session, where the docker group applies).
func (r *Registry) SetUpDocker(ctx context.Context, in Input, onLine func(string)) (Inspection, error) {
	t, err := sshTarget(in)
	if err != nil {
		return Inspection{}, err
	}
	c, err := r.dialSSH(ctx, t)
	if err != nil {
		return Inspection{}, err
	}
	insp, err := inspect(ctx, c)
	if err != nil {
		c.Close()
		return Inspection{}, err
	}
	if insp.Ready {
		c.Close()
		return insp, nil
	}
	if !insp.CanSetUp {
		c.Close()
		return insp, errors.New(insp.Reason)
	}
	cmd := "sh -c " + remote.Quote(insp.Script)
	if !insp.Root {
		cmd = "sudo -n " + cmd
	}
	r.opts.Log.Info("setting up Docker on a host", "host", t.Host, "distro", insp.Distro, "install", !insp.Docker)
	res, err := c.RunLive(ctx, cmd, nil, onLine)
	c.Close()
	if err != nil {
		return insp, err
	}
	if res.ExitCode != 0 {
		return insp, fmt.Errorf("setting Docker up failed (exit status %s); the output above says why", strconv.Itoa(res.ExitCode))
	}
	// The docker group only applies to new sessions: check in one.
	c, err = r.dialSSH(ctx, t)
	if err != nil {
		return insp, err
	}
	defer c.Close()
	after, err := inspect(ctx, c)
	if err != nil {
		return insp, err
	}
	// A saved host's connection was made before the group change.
	r.mu.Lock()
	if conn, ok := r.conns[in.ID]; ok && conn.ssh != nil {
		conn.ssh.reset()
	}
	r.mu.Unlock()
	if !after.Ready {
		return after, errors.New("Docker was set up, but " + after.User + " still can't use it")
	}
	r.opts.Log.Info("set up Docker on a host", "host", t.Host, "version", after.DockerVersion)
	return after, nil
}
