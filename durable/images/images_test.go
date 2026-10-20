package images

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/imagetest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/tools"
)

// Ported from pi packages/durable/test/tools-images.test.ts: read with the
// image processor, EXIF orientation, and the processor's concurrency. pi
// prepares in a worker thread; here Prepare runs on the calling goroutine, so
// its worker cases become "the same result whatever the goroutine, the
// caller's bytes untouched" and "concurrent images". n/a: "prepares in a
// worker thread, leaving the event loop free" (Go has no event loop to
// block), the two toBase64 cases (the standard encoding, no Buffer or node
// heap), and images-wasm-module (no WebAssembly).

var bg = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// api is the execution API of one read: the environment, and a model taking
// images within resize.
type api struct {
	harness.ToolExecutionAPI
	env   env.ExecutionEnv
	model *ai.Model
}

func (a *api) Env() env.ExecutionEnv                   { return a.env }
func (a *api) Output([]byte, *output.Skip) error       { return nil }
func (a *api) OutputWindow() *env.OutputWindow         { return nil }
func (a *api) Diagnostic(harness.ToolDiagnostic) error { return nil }
func (a *api) Details(context.Context, any) error      { return nil }
func (a *api) RetainedOutput() (string, bool, error)   { return "", false, nil }
func (a *api) Models() ai.Models                       { return oneModel{model: a.model} }
func (a *api) Agent(context.Context) (*harness.Agent, error) {
	return &harness.Agent{Model: &harness.ModelRef{Provider: "test", ModelID: "test"}}, nil
}

type oneModel struct {
	ai.Models
	model *ai.Model
}

func (m oneModel) GetModel(string, string) *ai.Model { return m.model }

// read writes data to name and reads it with tool (read with this package's
// processor when nil).
func read(t *testing.T, name string, data []byte, resize *ai.ModelImageResizeOptions, tool *harness.ToolRegistration) harness.ToolExecutionResult {
	t.Helper()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	model := &ai.Model{Input: []string{"text", "image"}}
	if resize != nil {
		model.InputLimits = &ai.ModelInputLimits{Images: &ai.ModelImageInputLimits{Resize: resize}}
	}
	if tool == nil {
		tool = tools.NewReadTool(tools.ReadToolOptions{Images: NewImages()})
	}
	result, err := tool.Execute(bg, map[string]any{"path": name}, &api{env: env.NewLocal(dir), model: model})
	must(t, err)
	return result
}

func messages(result harness.ToolExecutionResult) []string {
	var out []string
	for _, d := range result.Diagnostics {
		out = append(out, d.Message)
	}
	return out
}

func onlyImage(t *testing.T, result harness.ToolExecutionResult) ai.ImageContent {
	t.Helper()
	if len(result.Output) != 1 {
		t.Fatalf("output = %#v", result.Output)
	}
	image, ok := result.Output[0].(ai.ImageContent)
	if !ok {
		t.Fatalf("output = %#v", result.Output)
	}
	return image
}

func decoded(t *testing.T, data string) imagetest.Decoded {
	t.Helper()
	d, err := imagetest.Decode(data)
	must(t, err)
	return d
}

func prepare(t *testing.T, data []byte, mimeType string, limits tools.ImageLimits) *tools.PreparedImage {
	t.Helper()
	prepared, err := NewImages().Prepare(bg, data, mimeType, limits)
	must(t, err)
	return prepared
}

func TestImagesPassAnImageThatNeedsNoChangeThroughUntouched(t *testing.T) {
	png := imagetest.Encode(8, 6, "png")
	result := read(t, "a.png", png, nil, nil)
	if !reflect.DeepEqual(result.Output, ai.ContentList{ai.ImageContent{Data: base64.StdEncoding.EncodeToString(png), MimeType: "image/png"}}) {
		t.Fatalf("output = %#v", result.Output)
	}
	if got := onlyImage(t, read(t, "a.gif", imagetest.GIF(1, 1), nil, nil)).MimeType; got != "image/gif" {
		t.Fatalf("gif became %s", got)
	}
}

func TestImagesApplyTheModelsImageLimits(t *testing.T) {
	// The resized half of pi's "applies the model's image limits instead of
	// the defaults".
	four := 4
	image := onlyImage(t, read(t, "a.png", imagetest.Encode(8, 6, "png"), &ai.ModelImageResizeOptions{MaxWidth: &four}, nil))
	if d := decoded(t, image.Data); d.Width != 4 || d.Height != 3 {
		t.Fatalf("decoded %dx%d", d.Width, d.Height)
	}
}

func TestImagesScaleAnImageDownToTheMaximumSizeAndSayHowToMapCoordinatesBack(t *testing.T) {
	result := read(t, "wide.png", imagetest.Encode(4000, 1000, "png"), nil, nil)
	block := onlyImage(t, result)
	if d := decoded(t, block.Data); d.Width != 2000 || d.Height != 500 {
		t.Fatalf("decoded %dx%d", d.Width, d.Height)
	}
	want := fmt.Sprintf("Read image file [%s]. Resized from 4000x1000 to 2000x500. Multiply coordinates by 2.00 to map them to the original.", block.MimeType)
	if got := messages(result); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("messages = %q", got)
	}
}

func TestImagesReEncodeAPNGTooLargeForTheByteLimitAsJPEGAtItsSizeAndShrinkItWhenThatIsNotEnough(t *testing.T) {
	result := read(t, "big.png", imagetest.NoisyPNG(1100, 1100), nil, nil)
	jpeg := onlyImage(t, result)
	if jpeg.MimeType != "image/jpeg" {
		t.Fatalf("mime = %s", jpeg.MimeType)
	}
	if got := messages(result); !reflect.DeepEqual(got, []string{"Read image file [image/jpeg]. Converted from image/png to image/jpeg."}) {
		t.Fatalf("messages = %q", got)
	}
	if len(jpeg.Data) > tools.DefaultImageLimits.MaxBytes {
		t.Fatalf("%d bytes of base64", len(jpeg.Data))
	}
	if d := decoded(t, jpeg.Data); d.Width != 1100 || d.Height != 1100 {
		t.Fatalf("decoded %dx%d", d.Width, d.Height)
	}
	if raceDetector {
		// Encoding a noisy 2000x2000 image at every step takes minutes under
		// the race detector; the test step without it runs the rest.
		return
	}
	tight := tools.DefaultImageLimits
	tight.MaxBytes = 200_000
	shrunk := prepare(t, imagetest.NoisyPNG(2000, 2000), "image/png", tight)
	if shrunk == nil || len(shrunk.Data) > tight.MaxBytes || shrunk.Resized == nil ||
		shrunk.Resized.From != (tools.ImageSize{Width: 2000, Height: 2000}) || shrunk.Resized.To.Width >= 2000 {
		t.Fatalf("shrunk = %+v", shrunk)
	}
}

func TestImagesKeepAtLeastOnePixelOnEachSideOfAnExtremeAspectRatio(t *testing.T) {
	result := prepare(t, imagetest.Encode(6000, 2, "png"), "image/png", tools.DefaultImageLimits)
	want := &tools.ImageResize{From: tools.ImageSize{Width: 6000, Height: 2}, To: tools.ImageSize{Width: 2000, Height: 1}}
	if result == nil || !reflect.DeepEqual(result.Resized, want) {
		t.Fatalf("result = %+v", result)
	}
}

func TestImagesGiveUpWhenNotEvenOnePixelFitsTheByteLimit(t *testing.T) {
	limits := tools.DefaultImageLimits
	limits.MaxBytes = 8
	if result := prepare(t, imagetest.Encode(4, 4, "png"), "image/png", limits); result != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestImagesKeepAJPEGThatMustBeReEncodedAJPEG(t *testing.T) {
	result := prepare(t, imagetest.WithOrientation(imagetest.Encode(40, 30, "jpeg"), 3, false, false), "image/jpeg", tools.DefaultImageLimits)
	if result == nil || result.MimeType != "image/jpeg" {
		t.Fatalf("result = %+v", result)
	}
}

func TestImagesConvertAFormatProvidersDoNotTakeInlineToPNG(t *testing.T) {
	result := read(t, "a.bmp", imagetest.BMP(4, 4), nil, nil)
	if onlyImage(t, result).MimeType != "image/png" {
		t.Fatalf("output = %#v", result.Output)
	}
	if got := messages(result); !reflect.DeepEqual(got, []string{"Read image file [image/png]. Converted from image/bmp to image/png."}) {
		t.Fatalf("messages = %q", got)
	}
}

func TestImagesReportBytesTheyCannotDecodeAsAnError(t *testing.T) {
	broken := imagetest.Encode(4, 4, "png")[:40]
	result := read(t, "broken.png", broken, nil, nil)
	if !result.IsError || len(result.Output) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if got := messages(result); !reflect.DeepEqual(got, []string{"broken.png is an image (image/png) that cannot be prepared for the model"}) {
		t.Fatalf("messages = %q", got)
	}
}

func TestImagesAreWhatNewCodingToolsGivesRead(t *testing.T) {
	var readTool *harness.ToolRegistration
	for _, tool := range tools.NewCodingTools(tools.ReadToolOptions{Images: NewImages()}).Tools {
		if tool.Name == "read" {
			readTool = tool
		}
	}
	if onlyImage(t, read(t, "b.bmp", imagetest.BMP(4, 4), nil, readTool)).MimeType != "image/png" {
		t.Fatal("read of the coding tools did not convert the BMP")
	}
}

var (
	red    = imagetest.RGB{255, 0, 0}
	green  = imagetest.RGB{0, 255, 0}
	blue   = imagetest.RGB{0, 0, 255}
	white  = imagetest.RGB{255, 255, 255}
	black  = imagetest.RGB{0, 0, 0}
	yellow = imagetest.RGB{255, 255, 0}
)

func mirror(grid [][]imagetest.RGB) [][]imagetest.RGB {
	out := make([][]imagetest.RGB, len(grid))
	for y, row := range grid {
		for x := len(row) - 1; x >= 0; x-- {
			out[y] = append(out[y], row[x])
		}
	}
	return out
}

// turn is a quarter turn clockwise: the left column, read bottom to top,
// becomes the top row.
func turn(grid [][]imagetest.RGB) [][]imagetest.RGB {
	out := make([][]imagetest.RGB, len(grid[0]))
	for x := range grid[0] {
		for y := len(grid) - 1; y >= 0; y-- {
			out[x] = append(out[x], grid[y][x])
		}
	}
	return out
}

func TestImagesTurnEachEXIFOrientationUpright(t *testing.T) {
	stored := [][]imagetest.RGB{{red, green, blue}, {white, black, yellow}}
	// What each orientation shows, from its EXIF definition: mirror, then
	// turn clockwise.
	upright := map[int][][]imagetest.RGB{
		1: stored,
		2: mirror(stored),
		3: turn(turn(stored)),
		4: turn(turn(mirror(stored))),
		5: turn(turn(turn(mirror(stored)))),
		6: turn(stored),
		7: turn(mirror(stored)),
		8: turn(turn(turn(stored))),
	}
	for orientation := 1; orientation <= 8; orientation++ {
		t.Run(fmt.Sprintf("orientation %d", orientation), func(t *testing.T) {
			rgba, w, h := imagetest.Blocks(stored, 16)
			jpeg := imagetest.WithOrientation(imagetest.EncodeRGBA(rgba, w, h, "jpeg"), orientation, false, false)
			prepared := prepare(t, jpeg, "image/jpeg", tools.DefaultImageLimits)
			image := decoded(t, prepared.Data)
			expected := upright[orientation]
			if image.Width != len(expected[0])*16 || image.Height != len(expected)*16 {
				t.Fatalf("%dx%d", image.Width, image.Height)
			}
			// The colour at the centre of each block, rounded to full
			// channels against JPEG noise.
			for by, row := range expected {
				for bx, want := range row {
					at := ((by*16+8)*image.Width + bx*16 + 8) * 4
					var got imagetest.RGB
					for c := range 3 {
						if image.RGBA[at+c] > 127 {
							got[c] = 255
						}
					}
					if got != want {
						t.Errorf("block (%d,%d) = %v, want %v", bx, by, got, want)
					}
				}
			}
		})
	}
}

func TestExifOrientationReadsBigAndLittleEndianJPEGEXIFAfterAnAPP0SegmentAnd1WhenAbsentOrOutOfRange(t *testing.T) {
	jpeg := imagetest.Encode(2, 2, "jpeg")
	for _, c := range []struct {
		name string
		data []byte
		want int
	}{
		{"big-endian", imagetest.WithOrientation(jpeg, 6, false, false), 6},
		{"little-endian", imagetest.WithOrientation(jpeg, 8, true, false), 8},
		{"after APP0", imagetest.WithOrientation(jpeg, 5, false, true), 5},
		{"out of range", imagetest.WithOrientation(jpeg, 9, false, false), 1},
		{"absent", jpeg, 1},
		{"png", imagetest.Encode(2, 2, "png"), 1},
	} {
		if got := ExifOrientation(c.data); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestExifOrientationReadsWebPEXIFChunksWithAndWithoutTheExifPrefix(t *testing.T) {
	if got := ExifOrientation(imagetest.WebPWithOrientation(6, false)); got != 6 {
		t.Errorf("bare: %d", got)
	}
	if got := ExifOrientation(imagetest.WebPWithOrientation(3, true)); got != 3 {
		t.Errorf("prefixed: %d", got)
	}
}

func TestImagesGiveTheSameResultOnAnyGoroutineAndKeepTheCallersBytes(t *testing.T) {
	png := imagetest.NoisyPNG(1100, 1100)
	copied := bytes.Clone(png)
	here := prepare(t, png, "image/png", tools.DefaultImageLimits)
	var there *tools.PreparedImage
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		there, err = NewImages().Prepare(bg, png, "image/png", tools.DefaultImageLimits)
	}()
	<-done
	must(t, err)
	if !reflect.DeepEqual(here, there) {
		t.Fatal("results differ between goroutines")
	}
	if !bytes.Equal(png, copied) {
		t.Fatal("the caller's bytes changed")
	}
}

func TestImagesPrepareConcurrentImages(t *testing.T) {
	images := NewImages()
	inputs := []struct {
		data     []byte
		mimeType string
	}{
		{imagetest.BMP(4, 4), "image/bmp"},
		{imagetest.Encode(4000, 1000, "png"), "image/png"},
		{[]byte{1, 2, 3}, "image/png"},
	}
	results := make([]*tools.PreparedImage, len(inputs))
	var wg sync.WaitGroup
	for i, input := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := images.Prepare(bg, input.data, input.mimeType, tools.DefaultImageLimits)
			if err != nil {
				t.Error(err)
			}
			results[i] = result
		}()
	}
	wg.Wait()
	if results[0] == nil || results[0].MimeType != "image/png" {
		t.Errorf("bmp = %+v", results[0])
	}
	if results[1] == nil || results[1].Resized == nil || results[1].Resized.To != (tools.ImageSize{Width: 2000, Height: 500}) {
		t.Errorf("wide = %+v", results[1])
	}
	if results[2] != nil {
		t.Errorf("garbage = %+v", results[2])
	}
	// A later image on the same processor, as pi's after its idle worker
	// exited.
	if later := prepare(t, imagetest.BMP(2, 2), "image/bmp", tools.DefaultImageLimits); later == nil || later.MimeType != "image/png" {
		t.Errorf("later = %+v", later)
	}
}
