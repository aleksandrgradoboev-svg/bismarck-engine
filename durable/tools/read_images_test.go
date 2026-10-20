package tools

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/imagetest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/imagemime"
)

// Ported from pi packages/durable/test/tools-images.test.ts: read of images
// without an image processor, of an image that changes while it is read, and
// imageDimensions. The cases with the processor are in durable/images.

// modelAPI is a fakeAPI whose conversation model takes input within resize.
type modelAPI struct {
	*fakeAPI
	model *ai.Model
}

func (a *modelAPI) Agent(context.Context) (*harness.Agent, error) {
	return &harness.Agent{Model: &harness.ModelRef{Provider: "test", ModelID: "test"}}, nil
}
func (a *modelAPI) Models() ai.Models { return oneModel{model: a.model} }

// oneModel answers every lookup with its model.
type oneModel struct {
	ai.Models
	model *ai.Model
}

func (m oneModel) GetModel(string, string) *ai.Model { return m.model }

type imageReadOptions struct {
	images ImageProcessor
	input  []string
	resize *ai.ModelImageResizeOptions
	env    env.ExecutionEnv
	tool   *harness.ToolRegistration
}

// readImage writes data to name in dir and reads it for a model taking
// options.input (text and image by default).
func readImageFile(t *testing.T, dir, name string, data []byte, options imageReadOptions) (harness.ToolExecutionResult, error) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	input := options.input
	if input == nil {
		input = []string{"text", "image"}
	}
	model := &ai.Model{Input: input}
	if options.resize != nil {
		model.InputLimits = &ai.ModelInputLimits{Images: &ai.ModelImageInputLimits{Resize: options.resize}}
	}
	e := options.env
	if e == nil {
		e = env.NewLocal(dir)
	}
	tool := options.tool
	if tool == nil {
		tool = NewReadTool(ReadToolOptions{Images: options.images})
	}
	return tool.Execute(context.Background(), map[string]any{"path": name}, &modelAPI{fakeAPI: &fakeAPI{env: e}, model: model})
}

func messages(result harness.ToolExecutionResult) []string {
	var out []string
	for _, d := range result.Diagnostics {
		out = append(out, d.Message)
	}
	return out
}

func refused(t *testing.T, result harness.ToolExecutionResult) {
	t.Helper()
	if !result.IsError || result.Output == nil || len(result.Output) != 0 {
		t.Fatalf("result = %+v, want an error result without output", result)
	}
}

// readsEnv records the largest positional read of an opened file and shows
// each read to onRead.
type readsEnv struct {
	*env.Local
	mu      sync.Mutex
	largest int64
	onRead  func(offset, length int64)
}

type readsReader struct {
	env.BinaryReader
	e *readsEnv
}

func (e *readsEnv) OpenBinaryReader(ctx context.Context, path string, options env.BinaryReaderOptions) (env.BinaryReader, error) {
	reader, err := e.Local.OpenBinaryReader(ctx, path, options)
	if err != nil {
		return nil, err
	}
	return &readsReader{BinaryReader: reader, e: e}, nil
}

func (r *readsReader) Read(ctx context.Context, offset, length int64) ([]byte, error) {
	r.e.mu.Lock()
	r.e.largest = max(r.e.largest, length)
	r.e.mu.Unlock()
	b, err := r.BinaryReader.Read(ctx, offset, length)
	if r.e.onRead != nil {
		r.e.onRead(offset, length)
	}
	return b, err
}

func TestReadImagesReturnsASupportedImageWithinTheLimitsAsItIs(t *testing.T) {
	dir := t.TempDir()
	png := imagetest.Encode(8, 6, "png")
	result, err := readImageFile(t, dir, "a.png", png, imageReadOptions{})
	must(t, err)
	if !reflect.DeepEqual(result.Output, ai.ContentList{ai.ImageContent{Data: base64.StdEncoding.EncodeToString(png), MimeType: "image/png"}}) {
		t.Fatalf("output = %#v", result.Output)
	}
	if got := messages(result); !reflect.DeepEqual(got, []string{"Read image file [image/png]."}) {
		t.Fatalf("messages = %q", got)
	}
	tiny, err := readImageFile(t, dir, "a.gif", imagetest.GIF(1, 1), imageReadOptions{})
	must(t, err)
	if !reflect.DeepEqual(tiny.Output, ai.ContentList{ai.ImageContent{Data: imagetest.GIFData, MimeType: "image/gif"}}) {
		t.Fatalf("gif output = %#v", tiny.Output)
	}
}

func TestReadImagesRefusesAnImageTooLargeToSendWithoutReadingIt(t *testing.T) {
	dir := t.TempDir()
	e := &readsEnv{Local: env.NewLocal(dir)}
	result, err := readImageFile(t, dir, "big.png", imagetest.NoisyPNG(1100, 1100), imageReadOptions{env: e})
	must(t, err)
	refused(t, result)
	if !regexp.MustCompile(`^big\.png is an image \(image/png\) of .*, too large to send`).MatchString(messages(result)[0]) {
		t.Fatalf("message = %q", messages(result)[0])
	}
	// Only the header and the chunk walk, in blocks of at most 64 KiB.
	if e.largest > 64*1024 {
		t.Fatalf("largest read = %d", e.largest)
	}
}

func TestReadImagesRefusesAnImageWiderOrTallerThanTheLimitsByItsHeader(t *testing.T) {
	dir := t.TempDir()
	wide, err := readImageFile(t, dir, "wide.png", imagetest.Encode(2001, 2, "png"), imageReadOptions{})
	must(t, err)
	refused(t, wide)
	want := "wide.png is an image (image/png) of 2001x2, larger than 2000x2000, and no image processor is configured to shrink it"
	if got := messages(wide); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("messages = %q", got)
	}
	tall, err := readImageFile(t, dir, "tall.gif", imagetest.GIF(1, 3000), imageReadOptions{})
	must(t, err)
	if !strings.HasPrefix(messages(tall)[0], "tall.gif is an image (image/gif) of 1x3000, larger than 2000x2000") {
		t.Fatalf("message = %q", messages(tall)[0])
	}
}

func TestReadImagesRefusesAFormatThatNeedsConverting(t *testing.T) {
	result, err := readImageFile(t, t.TempDir(), "a.bmp", imagetest.BMP(4, 4), imageReadOptions{})
	must(t, err)
	refused(t, result)
	want := "a.bmp is an image (image/bmp) that needs converting, and no image processor is configured"
	if got := messages(result); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("messages = %q", got)
	}
}

func TestReadImagesSaysWhenTheModelSeesAPlaceholderInsteadOfTheImage(t *testing.T) {
	result, err := readImageFile(t, t.TempDir(), "a.png", imagetest.Encode(4, 4, "png"), imageReadOptions{input: []string{"text"}})
	must(t, err)
	if len(result.Output) != 1 {
		t.Fatalf("output = %#v", result.Output)
	}
	want := "Read image file [image/png]. The current model does not support images; it sees a placeholder instead."
	if got := messages(result); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("messages = %q", got)
	}
}

func TestReadImagesAppliesTheModelsImageLimitsInsteadOfTheDefaults(t *testing.T) {
	four := 4
	result, err := readImageFile(t, t.TempDir(), "a.png", imagetest.Encode(8, 6, "png"), imageReadOptions{resize: &ai.ModelImageResizeOptions{MaxWidth: &four}})
	must(t, err)
	if !strings.Contains(messages(result)[0], "of 8x6, larger than 4x2000,") {
		t.Fatalf("message = %q", messages(result)[0])
	}
	// The resized half of pi's case, with the processor, is in durable/images.
}

// stubImages is a processor that prepares nothing.
type stubImages struct{}

func (stubImages) Prepare(context.Context, []byte, string, ImageLimits) (*PreparedImage, error) {
	return nil, nil
}

func TestReadImagesListsBmpInTheDescriptionOnlyWithAProcessor(t *testing.T) {
	if d := NewReadTool(ReadToolOptions{}).Description; !strings.Contains(d, "(jpg, png, gif, webp)") {
		t.Fatalf("description = %q", d)
	}
	if d := NewReadTool(ReadToolOptions{Images: stubImages{}}).Description; !strings.Contains(d, "(jpg, png, gif, webp, bmp)") {
		t.Fatalf("description = %q", d)
	}
}

func appendTo(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.Write(data)
	must(t, f.Close())
	must(t, err)
}

func TestReadImagesReadsAnImageAgainRatherThanReturnACutOffImageEvenWhenItGrew(t *testing.T) {
	dir := t.TempDir()
	png := imagetest.Encode(8, 6, "png")
	path := filepath.Join(dir, "growing.png")
	var wholeReads []int64
	e := &readsEnv{Local: env.NewLocal(dir)}
	e.onRead = func(offset, length int64) {
		// A writer appends during the first read of the whole file; the
		// second read sees it all.
		if offset != 0 || length < int64(len(png)) {
			return
		}
		wholeReads = append(wholeReads, length)
		if len(wholeReads) == 1 {
			appendTo(t, path, make([]byte, 16))
		}
	}
	result, err := readImageFile(t, dir, "growing.png", png, imageReadOptions{env: e})
	must(t, err)
	if !reflect.DeepEqual(wholeReads, []int64{int64(len(png)), int64(len(png) + 16)}) {
		t.Fatalf("whole reads = %v", wholeReads)
	}
	after := append(append([]byte{}, png...), make([]byte, 16)...)
	if len(result.Output) != 1 || result.Output[0].(ai.ImageContent).Data != base64.StdEncoding.EncodeToString(after) {
		t.Fatalf("output = %#v", result.Output)
	}
}

func TestReadImagesFailsWhenItChangesDuringTheSecondReadToo(t *testing.T) {
	dir := t.TempDir()
	png := imagetest.Encode(8, 6, "png")
	path := filepath.Join(dir, "busy.png")
	e := &readsEnv{Local: env.NewLocal(dir)}
	e.onRead = func(offset, length int64) {
		if offset == 0 && length >= int64(len(png)) {
			appendTo(t, path, []byte{0})
		}
	}
	_, err := readImageFile(t, dir, "busy.png", png, imageReadOptions{env: e})
	if err == nil || err.Error() != "busy.png changed while it was read" {
		t.Fatalf("err = %v", err)
	}
}

func TestImageDimensionsReadsTheSizeEachFormatDeclaresInItsHeader(t *testing.T) {
	check := func(name string, data []byte, mimeType string, wantW, wantH int, wantOK bool) {
		t.Helper()
		w, h, ok := imagemime.Dimensions(data, mimeType)
		if w != wantW || h != wantH || ok != wantOK {
			t.Errorf("%s: %dx%d %v, want %dx%d %v", name, w, h, ok, wantW, wantH, wantOK)
		}
	}
	check("png", imagetest.Encode(7, 5, "png"), "image/png", 7, 5, true)
	check("jpeg", imagetest.Encode(7, 5, "jpeg"), "image/jpeg", 7, 5, true)
	check("jpeg with app0 and exif", imagetest.WithOrientation(imagetest.Encode(7, 5, "jpeg"), 6, false, true), "image/jpeg", 7, 5, true)
	check("gif", imagetest.GIF(300, 200), "image/gif", 300, 200, true)
	webp := func(kind string, data ...byte) []byte {
		out := append([]byte("RIFF"), 0, 0, 0, 0)
		out = append(out, "WEBP"+kind...)
		out = append(out, 0, 0, 0, 0)
		out = append(out, data...)
		return append(out, make([]byte, 16)...)
	}
	// VP8: frame tag, start code, then 14-bit width and height.
	check("vp8", webp("VP8 ", 0, 0, 0, 0x9d, 0x01, 0x2a, 0x2c, 0x01, 0xc8, 0x00), "image/webp", 300, 200, true)
	// VP8L: signature, then width - 1 and height - 1 in 14 bits each.
	bits := uint32(299 | 199<<14)
	check("vp8l", webp("VP8L", 0x2f, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)), "image/webp", 300, 200, true)
	// VP8X: flags, then width - 1 and height - 1 in 24 bits each.
	check("vp8x", webp("VP8X", 0, 0, 0, 0, 0x2b, 0x01, 0, 0xc7, 0, 0), "image/webp", 300, 200, true)
	check("truncated jpeg", []byte{0xff, 0xd8, 0xff}, "image/jpeg", 0, 0, false)
}
