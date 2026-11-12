package containers

type createConfiguration struct {
	Image        string              `json:"Image"`
	User         string              `json:"User"`
	WorkingDir   string              `json:"WorkingDir"`
	Entrypoint   []string            `json:"Entrypoint"`
	Cmd          []string            `json:"Cmd"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	Volumes      map[string]struct{} `json:"Volumes"`
	AttachStdin  bool                `json:"AttachStdin"`
	AttachStdout bool                `json:"AttachStdout"`
	AttachStderr bool                `json:"AttachStderr"`
	OpenStdin    bool                `json:"OpenStdin"`
	StdinOnce    bool                `json:"StdinOnce"`
	Tty          bool                `json:"Tty"`
	HostConfig   hostConfiguration   `json:"HostConfig"`
}

type hostConfiguration struct {
	AutoRemove     bool              `json:"AutoRemove"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	NetworkMode    string            `json:"NetworkMode"`
	PidMode        string            `json:"PidMode"`
	IpcMode        string            `json:"IpcMode"`
	CgroupnsMode   string            `json:"CgroupnsMode"`
	Privileged     bool              `json:"Privileged"`
	CapDrop        []string          `json:"CapDrop"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	Init           bool              `json:"Init"`
	VolumeDriver   string            `json:"VolumeDriver"`
	Tmpfs          map[string]string `json:"Tmpfs"`
	NanoCPUs       int64             `json:"NanoCpus"`
	Memory         int64             `json:"Memory"`
	MemorySwap     int64             `json:"MemorySwap"`
	PidsLimit      int64             `json:"PidsLimit"`
}

func (instance *Instance) configuration() createConfiguration {
	return createConfiguration{
		Image: instance.config.Image, User: "1000:1000", WorkingDir: "/home/commander",
		Entrypoint: []string{"/opt/hermes/.venv/bin/python"},
		Cmd: []string{"-I", "-S", "/usr/local/lib/commander-browser/hermes/container.py",
			"--source", "/opt/hermes", "--session", instance.config.Session,
			"--lightpanda", "/usr/local/bin/lightpanda", "--home", "/home/commander/state/worker", "--model-relay"},
		Env: []string{"HOME=/home/commander", "TMPDIR=/tmp", "PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1",
			"HERMES_DISABLE_LAZY_INSTALLS=1", "ANONYMIZED_TELEMETRY=false",
			"LIGHTPANDA_DISABLE_TELEMETRY=true", "LIGHTPANDA_DISABLE_CORE_DUMP=true"},
		Labels:      map[string]string{labelType: "hermes", labelSession: instance.config.Session, labelOwner: instance.nonce},
		Volumes:     map[string]struct{}{"/home/commander": {}},
		AttachStdin: true, AttachStdout: true, AttachStderr: true, OpenStdin: true, StdinOnce: true,
		HostConfig: hostConfiguration{ReadonlyRootfs: true, NetworkMode: "bridge", IpcMode: "private", CgroupnsMode: "private",
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Init: true, VolumeDriver: "local",
			Tmpfs:    map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=268435456,mode=0700,uid=1000,gid=1000"},
			NanoCPUs: 1_000_000_000, Memory: 1_000_000_000, MemorySwap: 1_000_000_000, PidsLimit: 128},
	}
}
