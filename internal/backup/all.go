package backup

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/s3"
)

// ServerBackups is one server's backups, for the list of every server's.
type ServerBackups struct {
	Server string `json:"server"`
	// Folders and the schedule are the plan's.
	Folders       []string `json:"folders"`
	IntervalHours int      `json:"intervalHours,omitempty"`
	At            *At      `json:"at,omitempty"`
	// Next is when the next scheduled backup is due, if one is.
	Next    *time.Time `json:"next,omitempty"`
	Running *Progress  `json:"running,omitempty"`
	Last    *Run       `json:"last,omitempty"`
	LastOK  *Run       `json:"lastOk,omitempty"`
	// Stored are its backups in the primary storage, newest first, and
	// StoredSize their total size; StoredError says why they couldn't be
	// listed.
	Stored      []s3.Object `json:"stored"`
	StoredSize  int64       `json:"storedSize"`
	StoredError string      `json:"storedError,omitempty"`
}

// AllBackups is every server's backups, and whatever is running now.
type AllBackups struct {
	Configured bool            `json:"configured"`
	Target     TargetRef       `json:"target"`
	Servers    []ServerBackups `json:"servers"`
	// Config is hlm's own settings backup, while one runs.
	Config *Progress `json:"config,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// All lists the backups of every server with a plan or a backup running
// that can see accepts (by server name). It needs nothing from Docker, so
// it answers even while a backup has a server's container busy. stored
// also lists each one's backups in the bucket.
func (s *Service) All(ctx context.Context, can func(server string) bool, stored bool) (AllBackups, error) {
	out := AllBackups{Servers: []ServerBackups{}}
	plans, err := s.plans()
	if err != nil {
		return out, err
	}
	byName := map[string]*ServerBackups{}
	add := func(name string) *ServerBackups {
		if sb := byName[name]; sb != nil {
			return sb
		}
		sb := &ServerBackups{Server: name, Folders: []string{}, Stored: []s3.Object{}}
		byName[name] = sb
		return sb
	}
	now, loc := time.Now(), s.loc()
	for name, p := range plans {
		if !can(name) || (len(p.Folders) == 0 && p.Last == nil) {
			continue
		}
		sb := add(name)
		sb.Folders = append(sb.Folders, p.Folders...)
		sb.IntervalHours, sb.At, sb.Last, sb.LastOK = p.IntervalHours, p.At, p.Last, p.LastOK
		if next := nextDue(p, now, loc); !next.IsZero() {
			sb.Next = &next
		}
	}
	s.mu.Lock()
	for name, prog := range s.running {
		cp := *prog
		if name == configRunName {
			if can("") {
				out.Config = &cp
			}
			continue
		}
		if can(name) {
			add(name).Running = &cp
		}
	}
	s.mu.Unlock()
	for _, sb := range byName {
		out.Servers = append(out.Servers, *sb)
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].Server < out.Servers[j].Server })

	ts, err := s.targets()
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	if len(ts) == 0 {
		return out, nil
	}
	out.Configured = true
	out.Target = TargetRef{ts[0].ID, ts[0].Name, ts[0].Encrypt}
	if !stored {
		return out, nil
	}
	st, err := s.store("")
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range out.Servers {
		sb := &out.Servers[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			objs, err := serverSeries(st.t.Settings, s.seriesName(sb.Server)).list(ctx, st.cl)
			if err != nil {
				sb.StoredError = err.Error()
				return
			}
			sb.Stored = newestFirst(objs)
			for _, o := range objs {
				sb.StoredSize += o.Size
			}
		}()
	}
	wg.Wait()
	return out, nil
}

// nextDue is when a plan's next scheduled backup comes due (now, if it's
// overdue), or zero when it has no schedule.
func nextDue(p Plan, now time.Time, loc *time.Location) time.Time {
	if len(p.Folders) == 0 {
		return time.Time{}
	}
	if due(p, now, loc) {
		return now
	}
	if p.At != nil {
		from := p.Since
		if p.Last != nil {
			from = p.Last.Started
		}
		if from.IsZero() {
			from = now
		}
		return p.At.next(from, loc)
	}
	if p.IntervalHours <= 0 || p.Last == nil {
		return time.Time{}
	}
	wait := time.Duration(p.IntervalHours) * time.Hour
	if p.Last.Error != "" && retryAfter < wait {
		wait = retryAfter
	}
	return p.Last.Started.Add(wait)
}
