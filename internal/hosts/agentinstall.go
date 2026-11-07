package hosts

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// Installing hlm's agent on a host over SSH: what the command on the Hosts
// page does, run by hlm. hlm makes the enrolment token, signs in, starts
// the hlm-agent container with it and waits for the agent to connect. From
// then on the host is reached through its agent, not SSH.

// AgentImage is the image the agent runs from (hlm's own).
const AgentImage = "danotchuuy/home-lab-manager:latest"

// AgentContainer is the agent's container's name on its host.
const AgentContainer = "hlm-agent"

// agentConnectWait is how long InstallAgent waits for the new agent to
// connect.
var agentConnectWait = 90 * time.Second

// AgentInput is a host to install hlm's agent on: the host as it'll be
// enrolled (ID, name, data folder, LAN address) and the SSH sign-in used
// to install it.
type AgentInput struct {
	Input
	// Server is hlm's address as the host reaches it, such as
	// https://hlm.example.com.
	Server string `json:"server"`
}

// agentServer checks hlm's address for the agent: http or https, with a
// host, without a trailing slash.
func agentServer(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("hlm's address should be like https://hlm.example.com")
	}
	return s, nil
}

// agentScript starts the agent's container, reading its enrolment token
// from stdin (so it's never on a command line, where sudo would log it). A
// container of that name for the same hlm (an earlier agent, perhaps for
// a host since removed) is replaced; one for another hlm is left alone.
func agentScript(server string, allowHTTP bool) string {
	flags := ""
	if allowHTTP {
		flags = " --allow-http"
	}
	return `set -e
IFS= read -r token
server=` + remote.Quote(server) + `
image=` + remote.Quote(AgentImage) + `
name=` + AgentContainer + `

if docker container inspect "$name" >/dev/null 2>&1; then
  old=$(docker container inspect -f '{{range .Args}}{{println .}}{{end}}' "$name" | sed -n '/^--server$/{n;p;q;}')
  if [ "$old" != "$server" ]; then
    echo "There's already an $name container here, for ${old:-another hlm}: remove it (docker rm -f $name) and try again" >&2
    exit 1
  fi
  echo "Replacing the $name container that was here"
  docker rm -f "$name" >/dev/null
fi

echo "Pulling $image"
docker pull -q "$image"

echo "Starting the agent"
docker run -d --name "$name" --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock -v hlm-agent:/data \
  -v /proc:/host/proc:ro -v /:/host/rootfs:ro \
  "$image" agent --server "$server"` + flags + ` --token "$token"
`
}

// InstallAgent signs in to a host over SSH, makes an enrolment token for
// it, starts hlm's agent there with it and waits for the agent to connect,
// passing lines of output to onLine. Docker has to be installed and running
// there, and the user able to use it (or be root, or have passwordless
// sudo).
func (r *Registry) InstallAgent(ctx context.Context, in AgentInput, onLine func(string)) (Host, error) {
	if r.opts.Agents == nil {
		return Host{}, errors.New("agents can't connect to this hlm")
	}
	t, err := sshTarget(in.Input)
	if err != nil {
		return Host{}, err
	}
	server, err := agentServer(in.Server)
	if err != nil {
		return Host{}, err
	}
	h := in.Host
	h.Kind, h.SSH, h.Socket, h.URL, h.CA, h.Cert = KindAgent, nil, "", "", "", ""
	if strings.TrimSpace(h.Address) == "" {
		h.Address = t.Host
	}
	// Check the host before signing in (CreateEnrolment checks it again).
	if err := validate(&h, ""); err != nil {
		return Host{}, err
	}
	if _, err := r.Get(h.ID); err == nil {
		return Host{}, fmt.Errorf("there's already a host %q", h.ID)
	}

	c, err := r.dialSSH(ctx, t)
	if err != nil {
		return Host{}, err
	}
	defer c.Close()
	insp, err := inspect(ctx, c)
	if err != nil {
		return Host{}, err
	}
	sudo := ""
	switch {
	case !insp.Docker || !insp.Running:
		return Host{}, errors.New("Docker isn't installed and running there yet: check for Docker and set it up first")
	case insp.Ready:
	case insp.Root:
	case insp.Sudo:
		sudo = "sudo -n "
	default:
		return Host{}, fmt.Errorf("%s can't use Docker there: add them to the docker group (check for Docker, and hlm can), or sign in as root", insp.User)
	}

	e, err := r.CreateEnrolment(h)
	if err != nil {
		return Host{}, err
	}
	r.opts.Log.Info("installing hlm's agent on a host", "host", h.ID, "ssh", t.Host, "server", server)
	res, err := c.RunLive(ctx, sudo+"sh -c "+remote.Quote(agentScript(server, strings.HasPrefix(server, "http://"))), []byte(e.Token+"\n"), onLine)
	if err != nil {
		return Host{}, err
	}
	if res.ExitCode != 0 {
		return Host{}, fmt.Errorf("starting the agent failed (exit status %d); the output above says why", res.ExitCode)
	}

	onLine("Waiting for the agent to connect to " + server)
	wait, cancel := context.WithTimeout(ctx, agentConnectWait)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for !r.opts.Agents.Status(h.ID).Connected {
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return Host{}, ctx.Err()
			}
			// What the agent says is usually why (hlm's address unreachable
			// from there, its certificate not trusted, …).
			if logs, err := c.Run(ctx, sudo+"docker logs --tail 15 "+AgentContainer, nil); err == nil {
				for _, l := range strings.Split(strings.TrimSpace(logs.Output()), "\n") {
					onLine(l)
				}
			}
			return Host{}, fmt.Errorf("the agent is running there, but hasn't connected to %s: can the host reach that address? Its log above may say why; it keeps trying, and the host appears here once it connects", server)
		case <-tick.C:
		}
	}
	r.opts.Log.Info("installed hlm's agent on a host", "host", h.ID)
	return r.Get(h.ID)
}
