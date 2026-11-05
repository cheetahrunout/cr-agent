package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// GPULayers is "all" or an integer 0..1024.
type GPULayers struct {
	All bool
	N   int
	set bool
}

// AllLayers returns gpu_layers "all".
func AllLayers() GPULayers { return GPULayers{All: true, set: true} }

// Layers returns gpu_layers n.
func Layers(n int) GPULayers { return GPULayers{N: n, set: true} }

// IsSet reports whether the value was present.
func (g GPULayers) IsSet() bool { return g.set }

func (g GPULayers) MarshalJSON() ([]byte, error) {
	if g.All {
		return []byte(`"all"`), nil
	}
	return json.Marshal(g.N)
}

func (g *GPULayers) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != "all" {
			return &FieldError{"gpu_layers", "must be \"all\" or an integer"}
		}
		*g = AllLayers()
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return &FieldError{"gpu_layers", "must be \"all\" or an integer"}
	}
	*g = Layers(n)
	return nil
}

// RawObject is a JSON object kept as raw bytes.
type RawObject []byte

func (r RawObject) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("{}"), nil
	}
	return r, nil
}

func (r *RawObject) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*r = nil
		return nil
	}
	if len(b) == 0 || b[0] != '{' {
		return errors.New("body must be a JSON object")
	}
	*r = append((*r)[:0], b...)
	return nil
}

// ErrUnknownType is returned (wrapped in *UnknownTypeError) for a frame type this agent does not know.
// The caller logs and drops the frame (ruling B2).
var ErrUnknownType = errors.New("unknown frame type")

// UnknownTypeError names the unknown type.
type UnknownTypeError struct{ T string }

func (e *UnknownTypeError) Error() string { return fmt.Sprintf("unknown frame type %q", e.T) }
func (e *UnknownTypeError) Unwrap() error { return ErrUnknownType }

// CmdError is a rejected command. The caller answers cmd.err{code, field} when ID is valid.
type CmdError struct {
	ID    string // empty when the id itself was unusable
	Op    string
	Code  string // bad_args or bad_setting
	Field string
	Msg   string
}

func (e *CmdError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("cmd %s: %s: %s (%s)", e.Op, e.Code, e.Msg, e.Field)
	}
	return fmt.Sprintf("cmd %s: %s: %s", e.Op, e.Code, e.Msg)
}

// ReqError is a rejected request. The caller answers res.head 400 + res.end error when ID is valid.
type ReqError struct {
	ID  string
	Msg string
}

func (e *ReqError) Error() string { return "req: " + e.Msg }

// FieldError is a value out of bounds.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

var (
	reULID    = regexp.MustCompile(`^[rc]-[0-9A-HJKMNP-TV-Z]{26}$`)
	reModelID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
	reRepo    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	reCommit  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reSeg     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,205}$`)
	reSemver  = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}$`)
	reTmpl    = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	reGPUSel  = regexp.MustCompile(`^(all|[0-7])$`)
)

// ValidID checks a hub-minted id (r- or c- ULID).
func ValidID(s string) bool { return reULID.MatchString(s) }

// ValidModelID checks a model id.
func ValidModelID(s string) bool { return reModelID.MatchString(s) }

// ValidSemver checks a version as update{version} carries it (ruling B23).
func ValidSemver(s string) bool { return reSemver.MatchString(s) }

// ValidRepo checks owner/repo, also refusing Windows device names and segments ending in '.' (ruling B17).
func ValidRepo(s string) bool {
	if !reRepo.MatchString(s) || strings.Contains(s, "..") {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if badSegment(seg) {
			return false
		}
	}
	return true
}

// ValidGGUFFile checks a file name: one or two segments, no "..", no segment ending in '.' or ' ',
// no Windows device names, suffix .gguf.
func ValidGGUFFile(s string) bool {
	if !strings.HasSuffix(s, ".gguf") || strings.Contains(s, "..") || strings.ContainsAny(s, `\:`) {
		return false
	}
	segs := strings.Split(s, "/")
	if len(segs) > 2 {
		return false
	}
	for _, seg := range segs {
		if !reSeg.MatchString(seg) || badSegment(seg) {
			return false
		}
	}
	return len(segs[len(segs)-1]) > len(".gguf")
}

var deviceNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}

func badSegment(seg string) bool {
	if seg == "" || strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return true
	}
	base := strings.ToUpper(seg)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if deviceNames[base] {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return true
	}
	return false
}

// BuiltinTemplates are llama.cpp's built-in chat template names that fit the schema pattern (b12125).
var BuiltinTemplates = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`bailing bailing-think bailing2 chatglm3 chatglm4 chatml command-r deepseek deepseek-ocr
		deepseek2 deepseek3 exaone-moe exaone3 exaone4 falcon3 gemma gigachat glmedge gpt-oss granite grok-2 hunyuan-dense
		hunyuan-moe hunyuan-vl kimi-k2 llama2 llama2-sys llama2-sys-bos llama2-sys-strip llama3 llama4 megrez minicpm mistral-v1
		mistral-v3 mistral-v3-tekken mistral-v7 mistral-v7-tekken monarch openchat orion pangu-embedded phi3 phi4 rwkv-world
		seed_oss smolvlm solar-open vicuna vicuna-orca yandex zephyr`) {
		BuiltinTemplates[n] = true
	}
}

// Validate checks the settings against the schema bounds (ruling B5). The supervisor applies the
// tighter, machine-specific bounds (logical cores, n_layer, n_ctx_train) on top.
func (s *Settings) Validate() *FieldError {
	switch {
	case s.Ctx < 4096 || s.Ctx > 262144:
		return &FieldError{"ctx", "must be 4096..262144"}
	case !s.GPULayers.IsSet():
		return &FieldError{"gpu_layers", "required"}
	case !s.GPULayers.All && (s.GPULayers.N < 0 || s.GPULayers.N > 1024):
		return &FieldError{"gpu_layers", "must be \"all\" or 0..1024"}
	case s.Threads < 1 || s.Threads > 256:
		return &FieldError{"threads", "must be 1..256"}
	case s.KVType != "f16" && s.KVType != "q8_0":
		return &FieldError{"kv_type", "must be f16 or q8_0"}
	case s.FlashAttn != "on" && s.FlashAttn != "auto":
		return &FieldError{"flash_attn", "must be on or auto"}
	case s.NCPUMoE != nil && (*s.NCPUMoE < 0 || *s.NCPUMoE > 1024):
		return &FieldError{"n_cpu_moe", "must be 0..1024"}
	case s.ChatTemplate != "" && (!reTmpl.MatchString(s.ChatTemplate) || !BuiltinTemplates[s.ChatTemplate]):
		return &FieldError{"chat_template", "must be a built-in template name"}
	case s.Parallel != nil && (*s.Parallel < 1 || *s.Parallel > 4):
		return &FieldError{"parallel", "must be 1..4"}
	}
	return nil
}

// strict decodes b into v rejecting unknown fields and trailing data.
func strict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

type head struct {
	V  *int   `json:"v"`
	T  string `json:"t"`
	ID string `json:"id"`
	Op string `json:"op"`
}

// Decode parses one frame. cmd and req are strict (ruling B2); every other known type is tolerant.
// Errors: *UnknownTypeError (log and drop), *CmdError, *ReqError, or a plain error for malformed JSON.
func Decode(b []byte) (Frame, error) {
	var h head
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("malformed frame: %w", err)
	}
	if h.V == nil || *h.V != 1 {
		return nil, errors.New("frame: v must be 1")
	}
	switch h.T {
	case TCmd:
		return decodeCmd(b, h)
	case TReq:
		return decodeReq(b, h)
	}
	var f Frame
	switch h.T {
	case THello:
		f = &Hello{}
	case TWelcome:
		f = &Welcome{}
	case TReject:
		f = &Reject{}
	case THW:
		f = &HW{}
	case TStatus:
		f = &Status{}
	case TCmdAck:
		f = &CmdAck{}
	case TCmdProgress:
		f = &CmdProgress{}
	case TCmdDone:
		f = &CmdDone{}
	case TCmdErr:
		f = &CmdErr{}
	case TReqBody:
		f = &ReqBody{}
	case TResHead:
		f = &ResHead{}
	case TResData:
		f = &ResData{}
	case TResEnd:
		f = &ResEnd{}
	case TCancel:
		f = &Cancel{}
	case TPing:
		f = &Ping{}
	case TPong:
		f = &Pong{}
	default:
		return nil, &UnknownTypeError{T: h.T}
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("frame %s: %w", h.T, err)
	}
	return f, nil
}

type cmdWire struct {
	Envelope
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

func decodeCmd(b []byte, h head) (Frame, error) {
	id := ""
	if ValidID(h.ID) {
		id = h.ID
	}
	bad := func(field, msg string) error {
		return &CmdError{ID: id, Op: h.Op, Code: ErrBadArgs, Field: field, Msg: msg}
	}
	var w cmdWire
	if err := strict(b, &w); err != nil {
		return nil, bad("", err.Error())
	}
	if id == "" {
		return nil, bad("id", "invalid id")
	}
	if len(w.Args) == 0 || bytes.Equal(bytes.TrimSpace(w.Args), []byte("null")) {
		return nil, bad("args", "required")
	}
	var args any
	var check func() error
	switch w.Op {
	case OpDownload:
		a := &DownloadArgs{}
		args, check = a, func() error {
			switch {
			case !ValidModelID(a.ModelID):
				return bad("model_id", "invalid")
			case !ValidRepo(a.Repo):
				return bad("repo", "invalid")
			case !reCommit.MatchString(a.Commit):
				return bad("commit", "invalid")
			case len(a.Files) < 1 || len(a.Files) > 16:
				return bad("files", "1..16 files")
			case a.Kind != "gguf" && a.Kind != "mmproj":
				return bad("kind", "must be gguf or mmproj")
			}
			for _, f := range a.Files {
				if !ValidGGUFFile(f) {
					return bad("files", "invalid file name")
				}
			}
			return nil
		}
	case OpInspect:
		a := &InspectArgs{}
		args, check = a, func() error {
			switch {
			case !ValidRepo(a.Repo):
				return bad("repo", "invalid")
			case !reCommit.MatchString(a.Commit):
				return bad("commit", "invalid")
			case !ValidGGUFFile(a.File):
				return bad("file", "invalid file name")
			}
			return nil
		}
	case OpLoad:
		a := &LoadArgs{}
		args, check = a, func() error {
			if !ValidModelID(a.ModelID) {
				return bad("model_id", "invalid")
			}
			if fe := a.Settings.Validate(); fe != nil {
				return &CmdError{ID: id, Op: h.Op, Code: ErrBadSetting, Field: fe.Field, Msg: fe.Msg}
			}
			return nil
		}
	case OpCancel:
		a := &IDArgs{}
		args, check = a, func() error {
			if !ValidID(a.ID) {
				return bad("id", "invalid")
			}
			return nil
		}
	case OpDelete:
		a := &ModelArgs{}
		args, check = a, func() error {
			if !ValidModelID(a.ModelID) {
				return bad("model_id", "invalid")
			}
			return nil
		}
	case OpUpdate:
		a := &UpdateArgs{}
		args, check = a, func() error {
			if !ValidSemver(a.Version) {
				return bad("version", "invalid")
			}
			return nil
		}
	case OpConfig:
		a := &ConfigArgs{}
		args, check = a, func() error {
			if a.GPUSelect != nil && !reGPUSel.MatchString(*a.GPUSelect) {
				return bad("gpu_select", "must be all or 0..7")
			}
			if a.IdleUnloadMin != nil && (*a.IdleUnloadMin < 0 || *a.IdleUnloadMin > 1440) {
				return &CmdError{ID: id, Op: h.Op, Code: ErrBadSetting, Field: "idle_unload_min", Msg: "must be 0..1440"}
			}
			return nil
		}
	case OpUnload, OpPause, OpResume:
		args, check = &EmptyArgs{}, func() error { return nil }
	default:
		return nil, bad("op", "unknown op")
	}
	if err := strict(w.Args, args); err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return nil, &CmdError{ID: id, Op: h.Op, Code: ErrBadSetting, Field: fe.Field, Msg: fe.Msg}
		}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && strings.HasPrefix(te.Field, "settings.") {
			return nil, &CmdError{ID: id, Op: h.Op, Code: ErrBadSetting, Field: strings.TrimPrefix(te.Field, "settings."), Msg: "wrong type"}
		}
		return nil, bad("args", err.Error())
	}
	if err := check(); err != nil {
		return nil, err
	}
	return &Cmd{Envelope: w.Envelope, ID: w.ID, Op: w.Op, Args: args}, nil
}

type reqWire struct {
	Envelope
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Stream    *bool     `json:"stream"`
	Body      RawObject `json:"body"`
	BodyParts *int      `json:"body_parts"`
}

func decodeReq(b []byte, h head) (Frame, error) {
	id := ""
	if ValidID(h.ID) {
		id = h.ID
	}
	var w reqWire
	if err := strict(b, &w); err != nil {
		return nil, &ReqError{ID: id, Msg: err.Error()}
	}
	switch {
	case id == "":
		return nil, &ReqError{Msg: "invalid id"}
	case w.Path != PathChat && w.Path != PathModels && w.Path != PathHealth:
		return nil, &ReqError{ID: id, Msg: "path not allowed"}
	case w.Stream == nil:
		return nil, &ReqError{ID: id, Msg: "stream required"}
	case w.BodyParts != nil && (*w.BodyParts < 1 || *w.BodyParts > 128):
		return nil, &ReqError{ID: id, Msg: "body_parts must be 1..128"}
	}
	return &Req{Envelope: w.Envelope, ID: w.ID, Path: w.Path, Stream: *w.Stream, Body: w.Body, BodyParts: w.BodyParts}, nil
}

// Encode serialises a frame, filling v and t.
func Encode(f Frame) ([]byte, error) {
	e := f.envelope()
	e.V = 1
	e.T = f.Type()
	return json.Marshal(f)
}
