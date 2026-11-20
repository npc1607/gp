package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gp/internal/tools"
)

func TestReadToolLoadsPNGImage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	imgPath := filepath.Join(dir, "sample.png")

	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(imgPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}

	rawArgs, _ := json.Marshal(readArgs{Path: imgPath})
	res, err := (&ReadTool{}).Execute(&tools.ToolContext{Context: context.Background(), Cwd: dir}, rawArgs)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected non-error result, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Image loaded: sample.png") {
		t.Fatalf("unexpected content: %q", res.Content)
	}
	if len(res.Images) != 1 {
		t.Fatalf("expected 1 image attachment, got %d", len(res.Images))
	}
	got := res.Images[0]
	if got.MimeType != "image/png" {
		t.Fatalf("expected mime image/png, got %q", got.MimeType)
	}
	if got.FileName != "sample.png" {
		t.Fatalf("expected filename sample.png, got %q", got.FileName)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Base64Data)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if !bytes.Equal(decoded, buf.Bytes()) {
		t.Fatalf("decoded image bytes mismatch")
	}
}

func TestReadToolRejectsOversizedImage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	imgPath := filepath.Join(dir, "huge.png")

	// 超过 5MB 上限
	data := make([]byte, maxReadImageBytes+1)
	copy(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err := os.WriteFile(imgPath, data, 0o644); err != nil {
		t.Fatalf("write huge png: %v", err)
	}

	rawArgs, _ := json.Marshal(readArgs{Path: imgPath})
	res, err := (&ReadTool{}).Execute(&tools.ToolContext{Context: context.Background(), Cwd: dir}, rawArgs)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for oversized image")
	}
	if len(res.Images) != 0 {
		t.Fatalf("expected 0 images on error, got %d", len(res.Images))
	}
}
