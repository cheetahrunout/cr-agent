// Package protocol holds the Go types of the agent link protocol (protocol/schema.json, proto 1.0)
// and the decoder that implements ruling B2: cmd and req are strict, every other frame is tolerant,
// and an unknown frame type is reported as ErrUnknownType so the caller can log and drop it.
package protocol

// Proto is the protocol version this agent speaks.
const Proto = "1.0"

// Frame type names.
const (
	THello       = "hello"
	TWelcome     = "welcome"
	TReject      = "reject"
	THW          = "hw"
	TStatus      = "status"
	TCmd         = "cmd"
	TCmdAck      = "cmd.ack"
	TCmdProgress = "cmd.progress"
	TCmdDone     = "cmd.done"
	TCmdErr      = "cmd.err"
	TReq         = "req"
	TReqBody     = "req.body"
	TResHead     = "res.head"
	TResData     = "res.data"
	TResEnd      = "res.end"
	TCancel      = "cancel"
	TPing        = "ping"
	TPong        = "pong"
)

// Envelope is embedded in every frame.
type Envelope struct {
	V int    `json:"v"`
	T string `json:"t"`
}

func (e *Envelope) envelope() *Envelope { return e }

// Frame is any protocol frame.
type Frame interface {
	Type() string
	envelope() *Envelope
}

// Pool is one memory pool of the hardware report.
type Pool struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // dedicated | unified | host
	Name    string `json:"name,omitempty"`
	TotalMB int64  `json:"total_mb"`
	FreeMB  int64  `json:"free_mb"`
	Backend string `json:"backend,omitempty"` // cuda | metal | vulkan | cpu
	Display *bool  `json:"display,omitempty"`
}

// CPU describes the processor.
type CPU struct {
	Name     string `json:"name,omitempty"`
	Physical int    `json:"physical,omitempty"`
	Logical  int    `json:"logical,omitempty"`
}

// Hardware is the hardware object of hello and hw.
type Hardware struct {
	Pools       []Pool `json:"pools"`
	CPU         CPU    `json:"cpu"`
	RAMMB       int64  `json:"ram_mb"`
	DiskFreeMB  int64  `json:"disk_free_mb"`
	DiskTotalMB int64  `json:"disk_total_mb,omitempty"`
	OS          string `json:"os"`
	Driver      string `json:"driver,omitempty"`
	CUDA        string `json:"cuda,omitempty"`
	OnBattery   *bool  `json:"on_battery,omitempty"`
}

// Engine states.
const (
	EngineUnloaded  = "unloaded"
	EngineLoading   = "loading"
	EngineLoaded    = "loaded"
	EngineUnloading = "unloading"
	EngineError     = "error"
)

// Job is an agent-owned long job keyed by model_id (ruling B3).
type Job struct {
	ModelID       string   `json:"model_id"`
	Kind          string   `json:"kind"`  // download | inspect | load | approval
	State         string   `json:"state"` // running | awaiting_click | paused | done | failed | denied
	Pct           *float64 `json:"pct,omitempty"`
	BytesDone     *int64   `json:"bytes_done,omitempty"`
	BytesTotal    *int64   `json:"bytes_total,omitempty"`
	AwaitingClick *bool    `json:"awaiting_click,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// LocalFile is one file of a local model.
type LocalFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// LocalModel is a model on disk.
type LocalModel struct {
	ModelID   string      `json:"model_id"`
	Repo      string      `json:"repo"`
	Commit    string      `json:"commit"`
	Files     []LocalFile `json:"files,omitempty"`
	Kind      string      `json:"kind,omitempty"`
	State     string      `json:"state"`
	SizeBytes *int64      `json:"size_bytes,omitempty"`
}

// Settings are the bounded engine settings of load (ruling B5).
type Settings struct {
	Ctx          int       `json:"ctx"`
	GPULayers    GPULayers `json:"gpu_layers"`
	Threads      int       `json:"threads"`
	KVType       string    `json:"kv_type"`
	FlashAttn    string    `json:"flash_attn"`
	NCPUMoE      *int      `json:"n_cpu_moe,omitempty"`
	ChatTemplate string    `json:"chat_template,omitempty"`
	Parallel     *int      `json:"parallel,omitempty"`
}

// Hello is the agent's first frame on every connection.
type Hello struct {
	Envelope
	AgentVersion         string       `json:"agent_version"`
	Proto                string       `json:"proto"`
	Platform             string       `json:"platform"`
	Backend              string       `json:"backend"`
	Capabilities         []string     `json:"capabilities"`
	SupportedArchs       []string     `json:"supported_archs"`
	LlamaTag             string       `json:"llama_tag"`
	Hardware             Hardware     `json:"hardware"`
	Engine               string       `json:"engine"`
	ModelID              string       `json:"model_id,omitempty"`
	SettingsInUse        *Settings    `json:"settings_in_use,omitempty"`
	Models               []LocalModel `json:"models"`
	Jobs                 []Job        `json:"jobs"`
	Inflight             []string     `json:"inflight"`
	LastDisconnectReason string       `json:"last_disconnect_reason,omitempty"`
	LocalPause           bool         `json:"local_pause"`
	HFToken              string       `json:"hf_token,omitempty"`
	LatestVerified       string       `json:"latest_verified_release,omitempty"`
	DriverOK             *bool        `json:"driver_ok,omitempty"`
}

// Limits are the hub's limits in welcome.
type Limits struct {
	MaxFrame int `json:"max_frame,omitempty"`
	MaxBody  int `json:"max_body,omitempty"`
}

// Welcome is the hub's answer to hello.
type Welcome struct {
	Envelope
	Proto           string `json:"proto"`
	Mode            string `json:"mode"` // normal | update_required
	PingIntervalS   int    `json:"ping_interval_s"`
	Limits          Limits `json:"limits"`
	HubVersion      string `json:"hub_version,omitempty"`
	MinAgentVersion string `json:"min_agent_version,omitempty"`
}

// Reject is sent before the hub closes.
type Reject struct {
	Envelope
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

// HW is a hardware report.
type HW struct {
	Envelope
	Hardware Hardware `json:"hardware"`
}

// UpdateStatus is the progress of a self-update.
type UpdateStatus struct {
	State   string   `json:"state,omitempty"`
	Pct     *float64 `json:"pct,omitempty"`
	Version string   `json:"version,omitempty"`
}

// Status reports engine and job state changes.
type Status struct {
	Envelope
	Engine         string        `json:"engine"`
	ModelID        string        `json:"model_id,omitempty"`
	EngineError    string        `json:"engine_error,omitempty"`
	LoadPct        *float64      `json:"load_pct,omitempty"`
	SettingsInUse  *Settings     `json:"settings_in_use,omitempty"`
	MeasuredVRAMMB *int64        `json:"measured_vram_mb,omitempty"`
	Jobs           []Job         `json:"jobs"`
	Models         []LocalModel  `json:"models,omitempty"`
	LocalPause     bool          `json:"local_pause"`
	Sleeping       *bool         `json:"sleeping,omitempty"`
	GPUPressureMB  *int64        `json:"gpu_pressure_mb,omitempty"`
	Update         *UpdateStatus `json:"update,omitempty"`
}

// Command ops (ruling B3, closed set).
const (
	OpDownload = "download"
	OpInspect  = "inspect"
	OpLoad     = "load"
	OpUnload   = "unload"
	OpPause    = "pause"
	OpResume   = "resume"
	OpCancel   = "cancel"
	OpDelete   = "delete"
	OpUpdate   = "update"
	OpConfig   = "config"
)

// Cmd is a command from the hub. After Decode, Args holds the validated args as a pointer to one
// of the *Args types below (by op); it is re-emitted as JSON.
type Cmd struct {
	Envelope
	ID   string `json:"id"`
	Op   string `json:"op"`
	Args any    `json:"args"`
}

// DownloadArgs are the args of download.
type DownloadArgs struct {
	ModelID string   `json:"model_id"`
	Repo    string   `json:"repo"`
	Commit  string   `json:"commit"`
	Files   []string `json:"files"`
	Kind    string   `json:"kind"`
}

// InspectArgs are the args of inspect.
type InspectArgs struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	File   string `json:"file"`
}

// LoadArgs are the args of load.
type LoadArgs struct {
	ModelID  string   `json:"model_id"`
	Settings Settings `json:"settings"`
}

// IDArgs are the args of cancel.
type IDArgs struct {
	ID string `json:"id"`
}

// ModelArgs are the args of delete.
type ModelArgs struct {
	ModelID string `json:"model_id"`
}

// UpdateArgs are the args of update.
type UpdateArgs struct {
	Version string `json:"version"`
}

// ConfigArgs are the args of config.
type ConfigArgs struct {
	KeepAwake     *bool   `json:"keep_awake,omitempty"`
	GPUSelect     *string `json:"gpu_select,omitempty"`
	IdleUnloadMin *int    `json:"idle_unload_min,omitempty"`
}

// EmptyArgs are the args of unload, pause and resume.
type EmptyArgs struct{}

// CmdAck acknowledges a command.
type CmdAck struct {
	Envelope
	ID string `json:"id"`
}

// CmdProgress reports command progress.
type CmdProgress struct {
	Envelope
	ID         string   `json:"id"`
	Pct        *float64 `json:"pct,omitempty"`
	BytesDone  *int64   `json:"bytes_done,omitempty"`
	BytesTotal *int64   `json:"bytes_total,omitempty"`
	State      string   `json:"state,omitempty"`
}

// CmdDone finishes a command.
type CmdDone struct {
	Envelope
	ID     string         `json:"id"`
	Result map[string]any `json:"result,omitempty"`
}

// cmd.err codes.
const (
	ErrBadSetting       = "bad_setting"
	ErrBadArgs          = "bad_args"
	ErrInsufficientVRAM = "insufficient_vram"
	ErrNoSpace          = "no_space"
	ErrDenied           = "denied"
	ErrDownloadFailed   = "download_failed"
	ErrHashMismatch     = "hash_mismatch"
	ErrNotFound         = "not_found"
	ErrBusy             = "busy"
	ErrUnsupported      = "unsupported"
	ErrHubTooOld        = "hub_too_old"
	ErrUpdateFailed     = "update_failed"
	ErrEngineFailed     = "engine_failed"
	ErrCancelled        = "cancelled"
)

// CmdErr fails a command.
type CmdErr struct {
	Envelope
	ID      string `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
	NeedMB  *int64 `json:"need_mb,omitempty"`
	FreeMB  *int64 `json:"free_mb,omitempty"`
}

// Request paths (the enum).
const (
	PathChat   = "/v1/chat/completions"
	PathModels = "/v1/models"
	PathHealth = "/health"
)

// Req is a request to relay to the local engine. Body is kept raw; the relay re-whitelists it (ruling B7).
type Req struct {
	Envelope
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Stream    bool      `json:"stream"`
	Body      RawObject `json:"body,omitempty"`
	BodyParts *int      `json:"body_parts,omitempty"`
}

// ReqBody is one part of a chunked request body.
type ReqBody struct {
	Envelope
	ID   string `json:"id"`
	Seq  int    `json:"seq"`
	Data string `json:"data"`
}

// ResHead carries the local status and content type.
type ResHead struct {
	Envelope
	ID     string `json:"id"`
	Status int    `json:"status"`
	CType  string `json:"ctype,omitempty"`
}

// ResData carries raw SSE data payloads (or one piece of a non-stream body).
type ResData struct {
	Envelope
	ID string   `json:"id"`
	D  []string `json:"d"`
}

// res.end reasons.
const (
	ReasonDone      = "done"
	ReasonCancelled = "cancelled"
	ReasonError     = "error"
)

// ResEnd closes a response.
type ResEnd struct {
	Envelope
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

// Cancel aborts a request (r-) or a command (c-).
type Cancel struct {
	Envelope
	ID string `json:"id"`
}

// Ping is the hub's app-level keepalive.
type Ping struct {
	Envelope
	TS int64 `json:"ts"`
}

// Pong answers a ping with the same ts.
type Pong struct {
	Envelope
	TS int64 `json:"ts"`
}

func (*Hello) Type() string       { return THello }
func (*Welcome) Type() string     { return TWelcome }
func (*Reject) Type() string      { return TReject }
func (*HW) Type() string          { return THW }
func (*Status) Type() string      { return TStatus }
func (*Cmd) Type() string         { return TCmd }
func (*CmdAck) Type() string      { return TCmdAck }
func (*CmdProgress) Type() string { return TCmdProgress }
func (*CmdDone) Type() string     { return TCmdDone }
func (*CmdErr) Type() string      { return TCmdErr }
func (*Req) Type() string         { return TReq }
func (*ReqBody) Type() string     { return TReqBody }
func (*ResHead) Type() string     { return TResHead }
func (*ResData) Type() string     { return TResData }
func (*ResEnd) Type() string      { return TResEnd }
func (*Cancel) Type() string      { return TCancel }
func (*Ping) Type() string        { return TPing }
func (*Pong) Type() string        { return TPong }
