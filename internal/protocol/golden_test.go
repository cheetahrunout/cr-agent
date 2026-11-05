package protocol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const goldenDir = "../../protocol/golden"

func TestGoldenRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden frames found: %v", err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := Decode(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			out, err := Encode(f)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("re-emitted frame differs\nwant %s\ngot  %s", raw, out)
			}
		})
	}
}

func TestGoldenInvalid(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "invalid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no invalid golden frames found: %v", err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Strip the documentation field "_reason" so the frame is rejected for its real reason,
			// not because "_reason" is itself an unknown field of a strict frame.
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			reason, _ := m["_reason"].(string)
			delete(m, "_reason")
			clean, _ := json.Marshal(m)
			_, err = Decode(clean)
			if err == nil {
				t.Fatalf("accepted an invalid frame (%s)", reason)
			}
			var ce *CmdError
			var re *ReqError
			if !errors.As(err, &ce) && !errors.As(err, &re) {
				t.Fatalf("want a CmdError or ReqError, got %T: %v", err, err)
			}
			if ce != nil && ce.ID == "" {
				t.Fatalf("cmd error lost the id, so no cmd.err could be sent: %v", err)
			}
			t.Logf("rejected (%s): %v", reason, err)
		})
	}
}

func TestDecoderRule(t *testing.T) {
	// Unknown t: reported as ErrUnknownType, never fatal.
	_, err := Decode([]byte(`{"v":1,"t":"telemetry","x":1}`))
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("unknown t: got %v", err)
	}
	// Tolerant frames ignore unknown fields.
	f, err := Decode([]byte(`{"v":1,"t":"welcome","proto":"1.1","mode":"normal","ping_interval_s":25,"limits":{},"future":true}`))
	if err != nil {
		t.Fatalf("welcome with an extra field: %v", err)
	}
	if f.(*Welcome).Proto != "1.1" {
		t.Fatal("welcome not decoded")
	}
	if _, err := Decode([]byte(`{"v":1,"t":"ping","ts":5,"extra":"x"}`)); err != nil {
		t.Fatalf("ping with extra field: %v", err)
	}
	// Strict: an unknown top-level field on req or cmd is rejected.
	if _, err := Decode([]byte(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T3","path":"/health","stream":false,"x":1}`)); err == nil {
		t.Fatal("req with unknown field accepted")
	}
	if _, err := Decode([]byte(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T3","op":"unload","args":{},"x":1}`)); err == nil {
		t.Fatal("cmd with unknown field accepted")
	}
	// Empty-arg ops take {} only.
	if _, err := Decode([]byte(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T3","op":"unload","args":{"force":true}}`)); err == nil {
		t.Fatal("unload with args accepted")
	}
	// Wrong version.
	if _, err := Decode([]byte(`{"v":2,"t":"ping","ts":1}`)); err == nil {
		t.Fatal("v=2 accepted")
	}
}

func TestSettingsBounds(t *testing.T) {
	cases := []struct {
		settings, field string
	}{
		{`{"ctx":2048,"gpu_layers":"all","threads":8,"kv_type":"f16","flash_attn":"on"}`, "ctx"},
		{`{"ctx":300000,"gpu_layers":"all","threads":8,"kv_type":"f16","flash_attn":"on"}`, "ctx"},
		{`{"ctx":8192,"gpu_layers":"most","threads":8,"kv_type":"f16","flash_attn":"on"}`, "gpu_layers"},
		{`{"ctx":8192,"gpu_layers":-1,"threads":8,"kv_type":"f16","flash_attn":"on"}`, "gpu_layers"},
		{`{"ctx":8192,"gpu_layers":10,"threads":0,"kv_type":"f16","flash_attn":"on"}`, "threads"},
		{`{"ctx":8192,"gpu_layers":10,"threads":4,"kv_type":"q4_0","flash_attn":"on"}`, "kv_type"},
		{`{"ctx":8192,"gpu_layers":10,"threads":4,"kv_type":"f16","flash_attn":"off"}`, "flash_attn"},
		{`{"ctx":8192,"gpu_layers":10,"threads":4,"kv_type":"f16","flash_attn":"on","chat_template":"evil"}`, "chat_template"},
		{`{"ctx":8192,"gpu_layers":10,"threads":4,"kv_type":"f16","flash_attn":"on","parallel":5}`, "parallel"},
	}
	for _, c := range cases {
		frame := `{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T1","op":"load","args":{"model_id":"m","settings":` + c.settings + `}}`
		_, err := Decode([]byte(frame))
		var ce *CmdError
		if !errors.As(err, &ce) {
			t.Fatalf("%s: want CmdError, got %v", c.settings, err)
		}
		if ce.Code != ErrBadSetting || ce.Field != c.field {
			t.Fatalf("%s: got code=%s field=%s, want bad_setting %s", c.settings, ce.Code, ce.Field, c.field)
		}
	}
}

func TestFileNames(t *testing.T) {
	good := []string{"Qwen3-14B-Q4_K_M.gguf", "sub/model-00001-of-00002.gguf", "mmproj-F16.gguf"}
	bad := []string{"../x.gguf", "a/b/c.gguf", "x.bin", "CON.gguf", "a./b.gguf", "a/..gguf", ".gguf", `a\b.gguf`, "lpt1/x.gguf"}
	for _, s := range good {
		if !ValidGGUFFile(s) {
			t.Errorf("rejected good %q", s)
		}
	}
	for _, s := range bad {
		if ValidGGUFFile(s) {
			t.Errorf("accepted bad %q", s)
		}
	}
}
