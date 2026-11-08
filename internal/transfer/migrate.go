package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

// Moving a server to another Docker host: a clone under the same name on
// the other host, with all its data, after which everything kept by the
// server's name moves to its new one (name@host) and the original is
// removed. Its data folders stay on the old host, listed on the Host page
// for deleting once the move is known to be good.

// MigrateOptions say where to move a server.
type MigrateOptions struct {
	Host string `json:"host"`
	// Request is its settings on the new host, from the move form (with the
	// network and address chosen there); nil uses PreviewMigrate's.
	Request *deploy.Request `json:"request,omitempty"`
}

// MigratePreview is how a server would move.
type MigratePreview struct {
	Preview
	// Name is its name on the new host.
	Name string `json:"name"`
	// Running says it runs now: it's stopped while its data is copied, and
	// started on the new host.
	Running bool `json:"running"`
	// Network is set when the server's network or address doesn't carry
	// over to the new host, so they have to be chosen there.
	Network *NetworkChoice `json:"network,omitempty"`
}

// NetworkChoice is why a moved server's network or address needs choosing.
type NetworkChoice struct {
	// Network, Subnets and IPAddress are the server's on its host now.
	Network   string   `json:"network"`
	Subnets   []string `json:"subnets"`
	IPAddress string   `json:"ipAddress,omitempty"`
	// Target are the network's subnets on the new host, if it has it.
	Target []string `json:"target,omitempty"`
	Reason string   `json:"reason"`
}

// Switchover moves what's kept by a server's name to its new one; Migrate
// calls it once the server runs on the new host.
type Switchover func(ctx context.Context, from, to string, emit func(manager.Event))

// PreviewMigrate works out how a server would move to host: the same name
// and settings there, its data folders under that host's appdata folder,
// its host ports moved only if they're taken there, and whether its
// network and address need choosing again.
func (s *Service) PreviewMigrate(ctx context.Context, id, host string) (MigratePreview, error) {
	src, err := s.opts.Manager.Inspect(ctx, id)
	if err != nil {
		return MigratePreview{}, err
	}
	srcHost, bare := manager.SplitID(strings.TrimPrefix(src.Name, "/"))
	if host == "" {
		return MigratePreview{}, errors.New("choose the Docker host to move it to")
	}
	if host == srcHost {
		return MigratePreview{}, fmt.Errorf("%s is on that host already", bare)
	}
	if stack := src.Config.Labels[manager.LabelStack]; stack != "" {
		return MigratePreview{}, fmt.Errorf("%s is part of the Compose stack %s, which can't move between hosts", bare, stack)
	}
	name := manager.JoinID(host, bare)
	if err := s.checkNewName(ctx, name); err != nil {
		return MigratePreview{}, err
	}
	e, err := s.Export(ctx, id, true)
	if err != nil {
		return MigratePreview{}, err
	}
	app, missing, err := s.template(e, host)
	if err != nil {
		return MigratePreview{}, err
	}
	root := strings.TrimRight(s.opts.Manager.AppdataRootOf(host), "/")
	req := localPaths(e.Request, root)
	req.Name = name
	p := MigratePreview{Name: name, Running: src.State.Running}
	p.App, p.TemplateMissing, p.Warnings, p.Settings = app, missing, []string{}, e.Settings.Summary()
	if p.Settings == nil {
		p.Settings = []string{}
	}

	network := req.Network
	if network == "" {
		network = app.Network
	}
	if choice, err := s.networkChoice(ctx, srcHost, host, network, req.IPAddress, bare); err != nil {
		return MigratePreview{}, err
	} else if choice != nil {
		p.Network = choice
		req.IPAddress = ""
	}
	if err := s.shiftPorts(ctx, app, &req, &p.Preview); err != nil {
		return MigratePreview{}, err
	}
	p.Request = req
	if p.Copies, err = s.copies(ctx, src, app, req); err != nil {
		return MigratePreview{}, err
	}
	for _, c := range p.Copies {
		if c.From == "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s is a Docker volume that isn't the server's own, so it can't be copied: the moved server gets its own, empty.", c.Target))
		}
	}
	p.Copies = slices.DeleteFunc(p.Copies, func(c Copy) bool { return c.From == "" })
	return p, nil
}

// builtinNetworks are Docker's own, on every host.
var builtinNetworks = []string{"", "bridge", "default", "host", "none"}

// networkChoice says whether a server on network (with a fixed address ip,
// if any) can keep them on the new host: the network has to be there, the
// address in its subnet and free, and a LAN network's subnet the same
// (otherwise its address, which players use, changes anyway).
func (s *Service) networkChoice(ctx context.Context, srcHost, host, network, ip, server string) (*NetworkChoice, error) {
	if slices.Contains(builtinNetworks, strings.ToLower(network)) {
		return nil, nil
	}
	choice := &NetworkChoice{Network: network, Subnets: []string{}, IPAddress: ip}
	if nets, err := s.opts.Manager.Networks(ctx, srcHost); err == nil {
		for _, n := range nets {
			if n.Name == network {
				choice.Subnets = n.Subnets
			}
		}
	}
	nets, err := s.opts.Manager.Networks(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("list the networks on %s: %w", host, err)
	}
	i := slices.IndexFunc(nets, func(n manager.NetworkInfo) bool { return n.Name == network })
	if i < 0 {
		choice.Reason = fmt.Sprintf("%s has no network called %s.", host, network)
		return choice, nil
	}
	dst := nets[i]
	choice.Target = dst.Subnets
	sameSubnets := slices.Equal(slices.Sorted(slices.Values(choice.Subnets)), slices.Sorted(slices.Values(dst.Subnets)))
	if ip != "" {
		addr, err := netip.ParseAddr(ip)
		in := false
		for _, sn := range dst.Subnets {
			if pf, perr := netip.ParsePrefix(sn); err == nil && perr == nil && pf.Contains(addr) {
				in = true
			}
		}
		if !in {
			choice.Reason = fmt.Sprintf("%s's subnet on %s is %s, so its address %s doesn't fit there.", network, host, strings.Join(dst.Subnets, ", "), ip)
			return choice, nil
		}
		for _, u := range dst.UsedIPs {
			if u.IP == ip && u.Container != server {
				choice.Reason = fmt.Sprintf("Its address %s is used by %s on %s.", ip, u.Container, host)
				return choice, nil
			}
		}
		return nil, nil
	}
	if dst.DirectIP && len(choice.Subnets) > 0 && !sameSubnets {
		choice.Reason = fmt.Sprintf("%s's subnet on %s is %s, not %s, so the server's address on your LAN changes.", network, host, strings.Join(dst.Subnets, ", "), strings.Join(choice.Subnets, ", "))
		return choice, nil
	}
	return nil, nil
}

// Migrate moves a server to another Docker host, with all its data: it's
// made there (stopped), the original is stopped and its data copied over,
// the new one is started if the original was running, switchover moves
// what's kept by the server's name to the new one, and the original is
// removed (its data folders stay, listed on the Host page). Until the new
// server runs, a failure removes it and starts the original again.
func (s *Service) Migrate(ctx context.Context, id string, opts MigrateOptions, switchover Switchover, emit func(manager.Event)) (manager.Server, error) {
	src, err := s.opts.Manager.Inspect(ctx, id)
	if err != nil {
		return manager.Server{}, err
	}
	srcName := strings.TrimPrefix(src.Name, "/")
	p, err := s.PreviewMigrate(ctx, id, opts.Host)
	if err != nil {
		return manager.Server{}, err
	}
	req := p.Request
	if opts.Request != nil {
		req = *opts.Request
		req.TemplateID, req.Name = p.Request.TemplateID, p.Name
		copies, err := s.copies(ctx, src, p.App, req)
		if err != nil {
			return manager.Server{}, err
		}
		p.Copies = slices.DeleteFunc(copies, func(c Copy) bool { return c.From == "" })
	} else if p.Network != nil {
		return manager.Server{}, errors.New(p.Network.Reason + " Choose its network and address on the new host.")
	}

	if p.TemplateMissing {
		// Removed from the catalog since: put the server's copy back.
		e, err := s.Export(ctx, id, false)
		if err != nil {
			return manager.Server{}, err
		}
		if err := s.opts.Catalog.AddCustom(e.Template); err != nil {
			return manager.Server{}, fmt.Errorf("add its template: %w", err)
		}
	}
	emit(manager.Event{Stage: "create", Message: "Making " + p.Name})
	srv, err := s.opts.Manager.Create(ctx, req, emit)
	if err != nil {
		return manager.Server{}, err
	}
	stopped := false
	fail := func(err error) (manager.Server, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if rerr := s.opts.Manager.Remove(rctx, srv.ID); rerr != nil {
			s.opts.Log.Warn("remove failed move", "server", srv.Name, "err", rerr)
		}
		if stopped {
			emit(manager.Event{Stage: "start", Message: "Starting " + srcName + " again where it was"})
			if serr := s.opts.Manager.Start(rctx, src.ID); serr != nil {
				emit(manager.Event{Stage: "warning", Message: fmt.Sprintf("Couldn't start %s again: %v", srcName, serr)})
			}
		}
		return manager.Server{}, err
	}

	if src.State.Running {
		emit(manager.Event{Stage: "stop", Message: "Stopping " + srcName + " while its data is copied"})
		if err := s.opts.Manager.Stop(ctx, src.ID); err != nil {
			return fail(fmt.Errorf("stop %s: %w", srcName, err))
		}
		stopped = true
	}
	folders := make([]string, 0, len(p.Copies))
	for _, c := range p.Copies {
		folders = append(folders, c.Target)
	}
	if len(folders) == 0 {
		emit(manager.Event{Stage: "copy", Message: "There are no data folders to copy"})
	} else {
		emit(manager.Event{Stage: "copy", Message: "Copying " + strings.Join(folders, ", ") + " to " + opts.Host})
		st, err := s.opts.Manager.CopyData(ctx, src.ID, srv.ID, folders, progressEmitter(emit))
		if err != nil {
			return fail(fmt.Errorf("copy the data: %w", err))
		}
		reportCopy(emit, st.Files, st.Bytes, st.Warnings)
	}
	if src.State.Running {
		emit(manager.Event{Stage: "start", Message: "Starting " + srv.Name})
		if err := s.opts.Manager.Start(ctx, srv.ID); err != nil {
			return fail(fmt.Errorf("start it on %s: %w", opts.Host, err))
		}
	}

	// From here the server is the new one: nothing is undone.
	if switchover != nil {
		switchover(ctx, srcName, srv.Name, emit)
	}
	emit(manager.Event{Stage: "copy", Message: "Removing " + srcName + " (its data folders stay there, listed on the Host page)"})
	if err := s.opts.Manager.Remove(context.WithoutCancel(ctx), src.ID); err != nil {
		emit(manager.Event{Stage: "warning", Message: fmt.Sprintf("Couldn't remove %s: %v. Remove it yourself, so the two don't both run.", srcName, err)})
	}
	emit(manager.Event{Stage: "done", Message: srcName + " moved to " + opts.Host + " as " + srv.Name, Server: &srv})
	return srv, nil
}
