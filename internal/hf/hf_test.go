package hf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/hf/hftest"
)

const commit = "7c1e2a9d40b3f1e8a9c0d2b4e6f8a1c3e5d7f9b2"

func setup(t *testing.T, token string) (*hftest.Server, *Client, []byte) {
	t.Helper()
	srv := hftest.New()
	t.Cleanup(srv.Close)
	data := make([]byte, 300_000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	files := map[string][]byte{"Model-Q4_K_M.gguf": data, "README.md": []byte("hello"), "sub/part-00001-of-00002.gguf": data[:1000]}
	for i := 0; i < 7; i++ {
		files[fmt.Sprintf("extra-%d.gguf", i)] = []byte{byte(i)}
	}
	srv.AddRepo("unsloth/Tiny-GGUF", commit, false, files)
	srv.AddRepo("meta-llama/Gated-GGUF", commit, true, map[string][]byte{"g.gguf": data[:500]})
	c, err := New(Options{Transport: srv.Transport(), Token: func() string { return token }})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c, data
}

func TestRevisionAndTree(t *testing.T) {
	srv, c, data := setup(t, "")
	ctx := context.Background()
	rev, err := c.Revision(ctx, "unsloth/tiny-gguf", commit) // the hub's spelling; HF answers with its own
	if err != nil || rev.ID != "unsloth/Tiny-GGUF" || rev.SHA != commit {
		t.Fatalf("%+v %v", rev, err)
	}
	var se *StatusError
	if _, err := c.Revision(ctx, "unsloth/absent", commit); !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("absent repo: %v", err)
	}
	srv.PageSize = 3 // forces four pages
	files, err := c.Tree(ctx, "unsloth/Tiny-GGUF", commit)
	if err != nil {
		t.Fatal(err)
	}
	f := files["Model-Q4_K_M.gguf"]
	if len(files) != 10 || f.Size != int64(len(data)) || f.SHA256 != hftest.SHA256(data) {
		t.Fatalf("%d files, model %+v", len(files), f)
	}
	if files["README.md"].SHA256 != "" || files["sub/part-00001-of-00002.gguf"].Size != 1000 {
		t.Fatalf("tree %+v", files)
	}
}

func TestOpenRangeAndRedirect(t *testing.T) {
	srv, c, data := setup(t, "hf_secret")
	ctx := context.Background()
	b, err := c.Open(ctx, "unsloth/Tiny-GGUF", commit, "Model-Q4_K_M.gguf", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(b)
	b.Close()
	if len(got) != len(data) || b.Total != int64(len(data)) || b.ETag == "" {
		t.Fatalf("full read %d bytes total %d etag %q", len(got), b.Total, b.ETag)
	}
	// Resume from the middle with If-Range.
	b2, err := c.Open(ctx, "unsloth/Tiny-GGUF", commit, "Model-Q4_K_M.gguf", 100_000, 0, b.ETag)
	if err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(b2)
	b2.Close()
	if b2.Offset != 100_000 || b2.Total != int64(len(data)) || len(rest) != 200_000 || rest[0] != data[100_000] {
		t.Fatalf("range read offset %d total %d len %d", b2.Offset, b2.Total, len(rest))
	}
	// A stale validator makes the server send the whole file from byte 0: the caller must restart.
	b3, err := c.Open(ctx, "unsloth/Tiny-GGUF", commit, "Model-Q4_K_M.gguf", 100_000, 0, `"stale"`)
	if err != nil || b3.Offset != 0 {
		t.Fatalf("stale If-Range: offset %d err %v", b3.Offset, err)
	}
	b3.Close()
	// A bounded range (inspect).
	b4, err := c.Open(ctx, "unsloth/Tiny-GGUF", commit, "Model-Q4_K_M.gguf", 0, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	head, _ := io.ReadAll(b4)
	b4.Close()
	if len(head) != 4096 || b4.Total != int64(len(data)) {
		t.Fatalf("bounded range %d bytes total %d", len(head), b4.Total)
	}
	// The token went to huggingface.co and never to the CDN host.
	if srv.TokenAtOrigin.Load() == 0 || srv.TokenAtCDN.Load() != 0 {
		t.Fatalf("token at origin %d, at CDN %d", srv.TokenAtOrigin.Load(), srv.TokenAtCDN.Load())
	}
}

func TestRedirectRules(t *testing.T) {
	srv, c, _ := setup(t, "hf_secret")
	ctx := context.Background()
	open := func() error {
		b, err := c.Open(ctx, "unsloth/Tiny-GGUF", commit, "Model-Q4_K_M.gguf", 0, 0, "")
		if err == nil {
			b.Close()
		}
		return err
	}
	srv.ExtraRedirects = 2 // resolve → hop → hop → blob = 3 hops: allowed
	if err := open(); err != nil {
		t.Fatalf("three hops refused: %v", err)
	}
	srv.ExtraRedirects = 3 // four hops
	if err := open(); !errors.Is(err, ErrRedirect) {
		t.Fatalf("four hops: %v", err)
	}
	srv.ExtraRedirects = 0
	for _, to := range []string{"https://evil.example/blob", "http://cdn-lfs.hf.co/blob/x", "https://cdn-lfs.hf.co.evil.io/x", "http://127.0.0.1:1/x", "file:///c:/windows/x"} {
		srv.RedirectTo = to
		if err := open(); !errors.Is(err, ErrRedirect) {
			t.Errorf("redirect to %s: %v", to, err)
		}
	}
	if srv.TokenAtCDN.Load() != 0 {
		t.Fatal("the token left huggingface.co")
	}
}

func TestGatedAndToken(t *testing.T) {
	srv, anon, _ := setup(t, "")
	srv.Token = "hf_good"
	ctx := context.Background()
	var se *StatusError
	if _, err := anon.Open(ctx, "meta-llama/Gated-GGUF", commit, "g.gguf", 0, 0, ""); !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("gated without a token: %v", err)
	}
	withTok, _ := New(Options{Transport: srv.Transport(), Token: func() string { return "hf_good" }})
	b, err := withTok.Open(ctx, "meta-llama/Gated-GGUF", commit, "g.gguf", 0, 0, "")
	if err != nil {
		t.Fatalf("gated with a token: %v", err)
	}
	b.Close()
	for tok, want := range map[string]TokenState{"": TokenAbsent, "hf_good": TokenPresent, "hf_bad": TokenInvalid} {
		got, err := anon.CheckToken(ctx, tok)
		if err != nil || got != want {
			t.Errorf("token %q: %v %v, want %v", tok, got, err, want)
		}
	}
}

func TestContentRange(t *testing.T) {
	for in, want := range map[string][2]int64{"bytes 100-299/300": {100, 300}, "bytes 0-0/1": {0, 1}} {
		s, tot, ok := parseContentRange(in)
		if !ok || s != want[0] || tot != want[1] {
			t.Errorf("%q → %d %d %v", in, s, tot, ok)
		}
	}
	for _, bad := range []string{"", "bytes */300", "bytes 100-299/*", "items 0-1/2", "bytes 300-299/300", "bytes -1-5/10"} {
		if _, _, ok := parseContentRange(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
