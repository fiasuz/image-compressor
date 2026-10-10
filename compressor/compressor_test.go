package compressor

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestCompressPNG(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	seed := uint32(1)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			seed = seed*1664525 + 1013904223
			src.SetNRGBA(x, y, color.NRGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 255})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, src); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := CompressPNG(&output, bytes.NewReader(input.Bytes()), Options{Colors: 32}); err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	paletted, ok := decoded.(*image.Paletted)
	if !ok {
		t.Fatalf("got %T, want *image.Paletted", decoded)
	}
	if len(paletted.Palette) > 32 {
		t.Fatalf("palette has %d colors", len(paletted.Palette))
	}
	if output.Len() >= input.Len() {
		t.Fatalf("output did not shrink: %d -> %d bytes", input.Len(), output.Len())
	}
	t.Logf("compressed %d -> %d bytes", input.Len(), output.Len())
}

func TestLosslessPreservesEveryPixelAndNeverGrows(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 48, 32))
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 5), G: uint8(y * 7), B: uint8((x + y) * 3), A: uint8(100 + (x+y)%156),
			})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, src); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := CompressPNG(&output, bytes.NewReader(input.Bytes()), Options{Lossless: true}); err != nil {
		t.Fatal(err)
	}
	if output.Len() > input.Len() {
		t.Fatalf("lossless output grew: %d -> %d bytes", input.Len(), output.Len())
	}
	got, err := png.Decode(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			wantPixel := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			gotPixel := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA)
			if gotPixel != wantPixel {
				t.Fatalf("pixel (%d,%d) changed: got %#v, want %#v", x, y, gotPixel, wantPixel)
			}
		}
	}
}

func TestLosslessUsesExactPaletteWhenPossible(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	colors := []color.NRGBA{
		{R: 20, G: 40, B: 180, A: 255},
		{R: 0, G: 170, B: 235, A: 255},
		{R: 0, G: 0, B: 0, A: 0},
	}
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			src.SetNRGBA(x, y, colors[(x/16+y/16)%len(colors)])
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, src); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := CompressPNG(&output, bytes.NewReader(input.Bytes()), Options{Lossless: true}); err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.(*image.Paletted); !ok {
		t.Fatalf("got %T, want exact paletted PNG", decoded)
	}
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			want := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			got := color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA)
			if got != want {
				t.Fatalf("pixel (%d,%d) changed: got %#v, want %#v", x, y, got, want)
			}
		}
	}
}

func TestSmartMeetsQualityThreshold(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 96, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 96; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 2), G: uint8(y * 2), B: uint8((x + y) % 256), A: 255,
			})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, src); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := CompressPNG(&output, bytes.NewReader(input.Bytes()), Options{Smart: true, MinPSNR: 36}); err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if score := pixelPSNR(src, got); score < 36 {
		t.Fatalf("PSNR %.2f is below threshold", score)
	}
	if output.Len() > input.Len() {
		t.Fatalf("smart output grew: %d -> %d bytes", input.Len(), output.Len())
	}
}

func TestSmartJPEG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 128, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 128; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 2), B: uint8((x + y) / 2), A: 255})
		}
	}
	var input bytes.Buffer
	if err := jpeg.Encode(&input, src, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	original, err := jpeg.Decode(bytes.NewReader(input.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Compress(&output, bytes.NewReader(input.Bytes()), Options{Smart: true, MinPSNR: 37}); err != nil {
		t.Fatal(err)
	}
	got, err := jpeg.Decode(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if score := pixelPSNR(original, got); score < 37 {
		t.Fatalf("PSNR %.2f is below threshold", score)
	}
	if output.Len() > input.Len() {
		t.Fatalf("JPEG output grew: %d -> %d bytes", input.Len(), output.Len())
	}
}

func TestGIFIsPreserved(t *testing.T) {
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	frame.SetColorIndex(1, 0, 1)
	var input bytes.Buffer
	if err := gif.Encode(&input, frame, nil); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Compress(&output, bytes.NewReader(input.Bytes()), Options{Smart: true}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), input.Bytes()) {
		t.Fatal("GIF bytes changed")
	}
}

func TestTransparency(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 0})
	src.SetNRGBA(1, 0, color.NRGBA{255, 0, 0, 255})
	got, err := Quantize(src, Options{Colors: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a0 := got.At(0, 0).RGBA()
	_, _, _, a1 := got.At(1, 0).RGBA()
	if a0 != 0 || a1 != 0xffff {
		t.Fatalf("alpha values = %d, %d", a0, a1)
	}
}

func TestInvalidColorCount(t *testing.T) {
	_, err := Quantize(image.NewRGBA(image.Rect(0, 0, 1, 1)), Options{Colors: 1})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPaletteSearchMatchesLinearScan(t *testing.T) {
	seed := uint32(1)
	next := func() uint8 {
		seed = seed*1664525 + 1013904223
		return uint8(seed >> 24)
	}
	for _, size := range []int{1, 2, 7, 16, 255, 256} {
		palette := make([]color.NRGBA, size)
		for i := range palette {
			palette[i] = color.NRGBA{R: next(), G: next(), B: next(), A: next()}
		}
		if size > 2 {
			// Duplicate entries force ties, which must resolve to the lowest index.
			palette[size-1], palette[size/2] = palette[0], palette[1]
		}
		search := newPaletteSearch(palette)
		for n := 0; n < 5000; n++ {
			r, g, b, a := next(), next(), next(), next()
			if n%5 == 0 {
				c := palette[int(r)%size]
				r, g, b, a = c.R, c.G, c.B, c.A
			}
			want, best := 0, 1<<62
			for i, c := range palette {
				dr, dg, db, da := int(r)-int(c.R), int(g)-int(c.G), int(b)-int(c.B), int(a)-int(c.A)
				if d := dr*dr + dg*dg + db*db + 2*da*da; d < best {
					want, best = i, d
				}
			}
			if got := search.nearest(r, g, b, a); int(got) != want {
				t.Fatalf("palette %d, color %v: got index %d, want %d", size, color.NRGBA{r, g, b, a}, got, want)
			}
		}
	}
}

func TestReferencePixelsMatchNRGBAModel(t *testing.T) {
	rect := image.Rect(3, 5, 19, 14)
	seed := uint32(7)
	next := func() uint8 {
		seed = seed*1664525 + 1013904223
		return uint8(seed >> 24)
	}
	rgba := image.NewRGBA(rect)
	nrgba := image.NewNRGBA(rect)
	gray := image.NewGray(rect)
	nrgba64 := image.NewNRGBA64(rect)
	ycbcr := image.NewYCbCr(rect, image.YCbCrSubsampleRatio420)
	paletted := image.NewPaletted(rect, color.Palette{
		color.NRGBA{R: 200, G: 10, B: 90, A: 128}, color.RGBA{R: 40, G: 40, B: 40, A: 255}, color.NRGBA{},
	})
	for i := range ycbcr.Y {
		ycbcr.Y[i] = next()
	}
	for i := range ycbcr.Cb {
		ycbcr.Cb[i], ycbcr.Cr[i] = next(), next()
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			a := next()
			rgba.SetRGBA(x, y, color.RGBA{R: next() % (a + 1), G: next() % (a + 1), B: next() % (a + 1), A: a})
			nrgba.SetNRGBA(x, y, color.NRGBA{R: next(), G: next(), B: next(), A: next()})
			gray.SetGray(x, y, color.Gray{Y: next()})
			nrgba64.SetNRGBA64(x, y, color.NRGBA64{R: uint16(next()) << 8, G: 0x1234, B: 0xfedc, A: uint16(next())<<8 | 0x7f})
			paletted.SetColorIndex(x, y, next()%3)
		}
	}
	for _, img := range []image.Image{rgba, nrgba, gray, nrgba64, ycbcr, paletted, nrgba.SubImage(image.Rect(5, 6, 9, 12))} {
		got := referencePixels(img)
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				want := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				i := 4 * ((y-b.Min.Y)*b.Dx() + (x - b.Min.X))
				if c := (color.NRGBA{R: got[i], G: got[i+1], B: got[i+2], A: got[i+3]}); c != want {
					t.Fatalf("%T pixel (%d,%d): got %v, want %v", img, x, y, c, want)
				}
			}
		}
	}
}

func TestLosslessPreserves16BitPixels(t *testing.T) {
	src := image.NewNRGBA64(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.SetNRGBA64(x, y, color.NRGBA64{R: 0x1234, G: 0x5678, B: 0x9abc, A: 0xffff})
		}
	}
	var input, output bytes.Buffer
	if err := png.Encode(&input, src); err != nil {
		t.Fatal(err)
	}
	if err := CompressPNG(&output, &input, Options{Lossless: true}); err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(&output)
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			want := src.NRGBA64At(x, y)
			actual := color.NRGBA64Model.Convert(got.At(x, y)).(color.NRGBA64)
			if actual != want {
				t.Fatalf("pixel (%d,%d): got %#v, want %#v", x, y, actual, want)
			}
		}
	}
}
