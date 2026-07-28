package protocol

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func testPNGDataURL(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		img.Set(x, x%height, color.RGBA{R: uint8(x), G: 80, B: 160, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestShrinkRequestImagesBoundsDimensions(t *testing.T) {
	url := testPNGDataURL(t, 3000, 2)
	body := []byte(`{"image_url":"` + url + `"}`)
	out, changed := shrinkRequestImages(body, 1<<20, 2048)
	if !changed {
		t.Fatal("expected oversized dimensions to be normalized")
	}
	root := unmarshalMap(t, out)
	_, data, ok := parseDataURL(strOpt(root["image_url"]))
	if !ok {
		t.Fatalf("output is not an image data URL: %s", out)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width > 2048 || cfg.Height > 2048 || len(raw) > 1<<20 {
		t.Fatalf("guard output = %dx%d, %d bytes", cfg.Width, cfg.Height, len(raw))
	}
}

// A rewritten image forces a full body re-marshal; >2^53 integer literals
// (snowflake-style ids) elsewhere in the body must survive that round trip.
func TestImageGuard_PreservesBigIntegersOnRewrite(t *testing.T) {
	url := testPNGDataURL(t, 3000, 2)
	body := []byte(`{"snowflake_id":9007199254740993,"nested":{"ids":[2535301200456458802,-9007199254740993]},"image_url":"` + url + `"}`)
	out, changed := shrinkRequestImages(body, 1<<20, 2048)
	if !changed {
		t.Fatal("expected oversized image to be rewritten")
	}
	wire := string(out)
	for _, literal := range []string{"9007199254740993", "2535301200456458802", "-9007199254740993"} {
		if !strings.Contains(wire, literal) {
			t.Errorf("rewritten body lost integer literal %s: %s", literal, wire)
		}
	}

	// Untouched bodies (no rewrite) stay byte-identical.
	plain := []byte(`{"snowflake_id":9007199254740993,"message":"no image"}`)
	got, ok := shrinkRequestImages(plain, 1<<20, 2048)
	if ok || !bytes.Equal(got, plain) {
		t.Fatalf("no-image body must stay byte-identical: %s, changed=%v", got, ok)
	}
}
