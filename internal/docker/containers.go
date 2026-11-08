package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// ContainerSummary is an entry from the container list endpoint.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
	Ports   []PortSummary     `json:"Ports"`
	Mounts  []MountPoint      `json:"Mounts"`
	// SizeRw and SizeRootFs are only filled in by ListContainersWithSize.
	SizeRw     int64 `json:"SizeRw"`
	SizeRootFs int64 `json:"SizeRootFs"`
	// HostConfig.NetworkMode is the network the container was created on.
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
}

// PortSummary is a published port as reported by the container list endpoint.
type PortSummary struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

// ContainerJSON is the subset of container inspect output that we use.
type ContainerJSON struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Image   string `json:"Image"`
	// RestartCount is how many times Docker has restarted the container
	// under its restart policy since it was started.
	RestartCount int `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health,omitempty"`
	} `json:"State"`
	Config struct {
		Image        string              `json:"Image"`
		Env          []string            `json:"Env"`
		Labels       map[string]string   `json:"Labels"`
		Tty          bool                `json:"Tty"`
		OpenStdin    bool                `json:"OpenStdin"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		// Healthcheck is the container's check: its own, or the image's.
		Healthcheck *Healthcheck `json:"Healthcheck,omitempty"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts     []MountPoint `json:"Mounts"`
	HostConfig struct {
		Binds         []string                 `json:"Binds"`
		NetworkMode   string                   `json:"NetworkMode"`
		PortBindings  map[string][]PortBinding `json:"PortBindings"`
		RestartPolicy RestartPolicy            `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// Healthcheck is a container's health check. An empty Test keeps the
// image's command; ["NONE"] turns the image's check off. Durations are in
// nanoseconds, and 0 means Docker's default.
type Healthcheck struct {
	Test        []string `json:"Test,omitempty"`
	Interval    int64    `json:"Interval,omitempty"`
	Timeout     int64    `json:"Timeout,omitempty"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
	Retries     int      `json:"Retries,omitempty"`
}

// CreateRequest is the body of a container create call.
type CreateRequest struct {
	Image        string              `json:"Image"`
	Hostname     string              `json:"Hostname,omitempty"`
	User         string              `json:"User,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Tty          bool                `json:"Tty,omitempty"`
	OpenStdin    bool                `json:"OpenStdin,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	// Volumes are anonymous volumes, by container path.
	Volumes     map[string]struct{} `json:"Volumes,omitempty"`
	Healthcheck *Healthcheck        `json:"Healthcheck,omitempty"`
	HostConfig  HostConfig          `json:"HostConfig"`
	// NetworkingConfig sets per-network options such as a static IP.
	NetworkingConfig *NetworkingConfig `json:"NetworkingConfig,omitempty"`
}

// HostConfig holds the host-specific parts of a container's configuration.
type HostConfig struct {
	Binds          []string                 `json:"Binds,omitempty"`
	PortBindings   map[string][]PortBinding `json:"PortBindings,omitempty"`
	NetworkMode    string                   `json:"NetworkMode,omitempty"`
	RestartPolicy  RestartPolicy            `json:"RestartPolicy"`
	Privileged     bool                     `json:"Privileged,omitempty"`
	Devices        []DeviceMapping          `json:"Devices,omitempty"`
	DeviceRequests []DeviceRequest          `json:"DeviceRequests,omitempty"`
	CapAdd         []string                 `json:"CapAdd,omitempty"`
	CapDrop        []string                 `json:"CapDrop,omitempty"`
	SecurityOpt    []string                 `json:"SecurityOpt,omitempty"`
	Sysctls        map[string]string        `json:"Sysctls,omitempty"`
	Tmpfs          map[string]string        `json:"Tmpfs,omitempty"`
	ExtraHosts     []string                 `json:"ExtraHosts,omitempty"`
	DNS            []string                 `json:"Dns,omitempty"`
	GroupAdd       []string                 `json:"GroupAdd,omitempty"`
	Ulimits        []Ulimit                 `json:"Ulimits,omitempty"`
	Memory         int64                    `json:"Memory,omitempty"`
	NanoCPUs       int64                    `json:"NanoCpus,omitempty"`
	CpusetCpus     string                   `json:"CpusetCpus,omitempty"`
	ShmSize        int64                    `json:"ShmSize,omitempty"`
	PidsLimit      *int64                   `json:"PidsLimit,omitempty"`
	Init           *bool                    `json:"Init,omitempty"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs,omitempty"`
	Runtime        string                   `json:"Runtime,omitempty"`
	LogConfig      *LogConfig               `json:"LogConfig,omitempty"`
}

// PortBinding maps a container port to a host address.
type PortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy controls what the daemon does when a container exits.
type RestartPolicy struct {
	Name              string `json:"Name,omitempty"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

// DeviceMapping passes a host device into the container.
type DeviceMapping struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

// DeviceRequest asks for devices such as GPUs from a device driver.
type DeviceRequest struct {
	Driver       string     `json:"Driver,omitempty"`
	Count        int        `json:"Count"`
	DeviceIDs    []string   `json:"DeviceIDs,omitempty"`
	Capabilities [][]string `json:"Capabilities"`
}

// Ulimit is a resource limit.
type Ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

// LogConfig selects the container log driver.
type LogConfig struct {
	Type   string            `json:"Type,omitempty"`
	Config map[string]string `json:"Config,omitempty"`
}

// ListContainers lists all containers (running or not) that carry every label
// in labels. Each label is "key" or "key=value".
func (c *Client) ListContainers(ctx context.Context, labels ...string) ([]ContainerSummary, error) {
	return c.listContainers(ctx, url.Values{"all": {"1"}}, labels)
}

// listContainers lists the containers with every label in labels, also
// finding those with the labels' old io.hlm.* names.
func (c *Client) listContainers(ctx context.Context, q url.Values, labels []string) ([]ContainerSummary, error) {
	list := func(labels []string) ([]ContainerSummary, error) {
		if len(labels) > 0 {
			f, _ := json.Marshal(map[string][]string{"label": labels})
			q.Set("filters", string(f))
		}
		var out []ContainerSummary
		err := c.doJSON(ctx, http.MethodGet, "/containers/json", q, nil, &out)
		return out, err
	}
	out, err := list(labels)
	if err != nil {
		return nil, err
	}
	if legacy, ok := legacyFilters(labels); ok {
		old, err := list(legacy)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(out))
		for _, s := range out {
			seen[s.ID] = true
		}
		for _, s := range old {
			if !seen[s.ID] {
				out = append(out, s)
			}
		}
	}
	for _, s := range out {
		addCurrentLabels(s.Labels)
	}
	return out, nil
}

// ListContainerByID returns the summary of one container, by full ID, or
// none if there's no such container.
func (c *Client) ListContainerByID(ctx context.Context, id string) ([]ContainerSummary, error) {
	f, _ := json.Marshal(map[string][]string{"id": {id}})
	var out []ContainerSummary
	err := c.doJSON(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &out)
	for _, s := range out {
		addCurrentLabels(s.Labels)
	}
	return out, err
}

// MountPoint is a bind mount or volume attached to a container.
type MountPoint struct {
	Type        string `json:"Type"` // bind, volume, tmpfs
	Name        string `json:"Name,omitempty"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// ListContainersWithSize is ListContainers with each container's disk usage
// filled in. The daemon has to measure it, so this is slow on big hosts.
func (c *Client) ListContainersWithSize(ctx context.Context, labels ...string) ([]ContainerSummary, error) {
	return c.listContainers(ctx, url.Values{"all": {"1"}, "size": {"1"}}, labels)
}

// DiskUsage is Docker's own disk usage: images, containers, volumes and
// build cache.
type DiskUsage struct {
	Images     int64 `json:"images"`
	Containers int64 `json:"containers"`
	Volumes    int64 `json:"volumes"`
	BuildCache int64 `json:"buildCache"`
	// VolumeSizes maps volume names to their size (-1 when unknown).
	VolumeSizes map[string]int64 `json:"-"`
}

// SystemDiskUsage returns Docker's disk usage summary.
func (c *Client) SystemDiskUsage(ctx context.Context) (DiskUsage, error) {
	var raw struct {
		LayersSize int64 `json:"LayersSize"`
		Containers []struct {
			SizeRw int64 `json:"SizeRw"`
		} `json:"Containers"`
		Volumes []struct {
			Name      string `json:"Name"`
			UsageData *struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
		BuildCache []struct {
			Size   int64 `json:"Size"`
			Shared bool  `json:"Shared"`
		} `json:"BuildCache"`
	}
	// Asking for every type at once can take much longer than asking for
	// each in turn (27s vs 5s on a test host), so ask separately.
	for _, typ := range []string{"image", "container", "volume", "build-cache"} {
		if err := c.doJSON(ctx, http.MethodGet, "/system/df", url.Values{"type": {typ}}, nil, &raw); err != nil {
			return DiskUsage{}, err
		}
	}
	du := DiskUsage{Images: raw.LayersSize, VolumeSizes: map[string]int64{}}
	for _, ct := range raw.Containers {
		du.Containers += ct.SizeRw
	}
	for _, v := range raw.Volumes {
		size := int64(-1)
		if v.UsageData != nil {
			size = v.UsageData.Size
		}
		du.VolumeSizes[v.Name] = size
		if size > 0 {
			du.Volumes += size
		}
	}
	for _, b := range raw.BuildCache {
		if !b.Shared {
			du.BuildCache += b.Size
		}
	}
	return du, nil
}

// InspectContainer returns details for a container by ID or name.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerJSON, error) {
	var out ContainerJSON
	err := c.doJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
	addCurrentLabels(out.Config.Labels)
	return out, err
}

// CreateContainer creates a container and returns its ID and any warnings.
func (c *Client) CreateContainer(ctx context.Context, name string, req CreateRequest) (string, []string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/containers/create", q, req, &out)
	return out.ID, out.Warnings, err
}

// StartContainer starts a container. Starting a running container is not an error.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.ignoreNotModified(c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil))
}

// StopContainer stops a container, waiting up to timeout seconds before
// killing it. A negative timeout uses the container's own stop timeout.
func (c *Client) StopContainer(ctx context.Context, id string, timeout int) error {
	q := url.Values{}
	if timeout >= 0 {
		q.Set("t", strconv.Itoa(timeout))
	}
	return c.ignoreNotModified(c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil))
}

// RestartContainer restarts a container. A negative timeout uses the
// container's own stop timeout.
func (c *Client) RestartContainer(ctx context.Context, id string, timeout int) error {
	q := url.Values{}
	if timeout >= 0 {
		q.Set("t", strconv.Itoa(timeout))
	}
	return c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/restart", q, nil, nil)
}

// RemoveContainer removes a container. Named volumes and bind mounts are kept.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	return c.doJSON(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil)
}

// WaitContainer waits for a container to stop and returns its exit code.
func (c *Client) WaitContainer(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait", nil, nil, &out); err != nil {
		return -1, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, errors.New(out.Error.Message)
	}
	return out.StatusCode, nil
}

// RemoveVolume removes a volume. The daemon refuses if a container uses it.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil)
}

// SetRestartPolicy changes a container's restart policy without recreating it.
func (c *Client) SetRestartPolicy(ctx context.Context, id, policy string) error {
	body := map[string]any{"RestartPolicy": RestartPolicy{Name: policy}}
	return c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/update", nil, body, nil)
}

// InspectContainerRaw returns a container's full inspect output, for cloning
// a container whose configuration this package doesn't model.
func (c *Client) InspectContainerRaw(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	err := c.doJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
	return out, err
}

// CreateContainerRaw creates a container from an arbitrary create body.
func (c *Client) CreateContainerRaw(ctx context.Context, name string, body any) (string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/containers/create", q, body, &out)
	return out.ID, err
}

// ConnectNetwork attaches a container to a network.
func (c *Client) ConnectNetwork(ctx context.Context, network, container string, endpoint any) error {
	body := map[string]any{"Container": container, "EndpointConfig": endpoint}
	return c.doJSON(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
}

// RenameContainer renames a container.
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

func (c *Client) ignoreNotModified(err error) error {
	if apiErr, ok := err.(*APIError); ok && apiErr.Status == http.StatusNotModified {
		return nil
	}
	return err
}

// LogsOptions controls which logs are returned.
type LogsOptions struct {
	Follow     bool
	Tail       string // number of lines, or "all"
	Timestamps bool
}

// Logs returns the container's combined stdout and stderr. The returned
// reader yields plain log text: the stream framing used for non-TTY
// containers is removed.
func (c *Client) Logs(ctx context.Context, id string, opts LogsOptions) (io.ReadCloser, error) {
	info, err := c.InspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if opts.Follow {
		q.Set("follow", "1")
	}
	if opts.Tail != "" {
		q.Set("tail", opts.Tail)
	}
	if opts.Timestamps {
		q.Set("timestamps", "1")
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	if info.Config.Tty {
		return resp.Body, nil
	}
	return newDemuxReader(resp.Body), nil
}

// Stats is a computed resource usage snapshot for a container.
type Stats struct {
	// CPUPercent is CPU use as `docker stats` reports it: 100% is one full
	// core, so it can go up to OnlineCPUs*100.
	CPUPercent float64 `json:"cpuPercent"`
	// OnlineCPUs is the number of CPUs available to the container.
	OnlineCPUs  uint32 `json:"onlineCpus"`
	MemoryUsage uint64 `json:"memoryUsage"`
	MemoryLimit uint64 `json:"memoryLimit"`
	NetworkRx   uint64 `json:"networkRx"`
	NetworkTx   uint64 `json:"networkTx"`
	BlockRead   uint64 `json:"blockRead"`
	BlockWrite  uint64 `json:"blockWrite"`
	PIDs        uint64 `json:"pids"`
}

type rawStats struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IOServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemCPUUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs     uint32 `json:"online_cpus"`
}

// ContainerStats returns a single resource usage sample for a running container.
func (c *Client) ContainerStats(ctx context.Context, id string) (Stats, error) {
	var raw rawStats
	// one-shot=false makes the daemon wait for a second sample, so precpu_stats
	// is filled in and the CPU percentage can be computed.
	q := url.Values{"stream": {"0"}}
	if err := c.doJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats", q, nil, &raw); err != nil {
		return Stats{}, err
	}

	var s Stats
	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(raw.CPUStats.SystemCPUUsage) - float64(raw.PreCPUStats.SystemCPUUsage)
	cpus := float64(raw.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(raw.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpuDelta > 0 && sysDelta > 0 {
		s.CPUPercent = cpuDelta / sysDelta * cpus * 100
	}
	s.OnlineCPUs = uint32(cpus)

	// Match `docker stats`: exclude page cache from memory usage.
	s.MemoryUsage = raw.MemoryStats.Usage
	if cache, ok := raw.MemoryStats.Stats["inactive_file"]; ok && cache < s.MemoryUsage {
		s.MemoryUsage -= cache
	} else if cache, ok := raw.MemoryStats.Stats["total_inactive_file"]; ok && cache < s.MemoryUsage {
		s.MemoryUsage -= cache
	}
	s.MemoryLimit = raw.MemoryStats.Limit

	for _, n := range raw.Networks {
		s.NetworkRx += n.RxBytes
		s.NetworkTx += n.TxBytes
	}
	for _, b := range raw.BlkioStats.IOServiceBytesRecursive {
		switch b.Op {
		case "read", "Read":
			s.BlockRead += b.Value
		case "write", "Write":
			s.BlockWrite += b.Value
		}
	}
	s.PIDs = raw.PidsStats.Current
	return s, nil
}

// DisconnectNetwork detaches a container from a network.
func (c *Client) DisconnectNetwork(ctx context.Context, network, container string, force bool) error {
	body := map[string]any{"Container": container, "Force": force}
	return c.doJSON(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", nil, body, nil)
}

// CreateVolume creates a named volume.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Labels": labels}
	return c.doJSON(ctx, http.MethodPost, "/volumes/create", nil, body, nil)
}
