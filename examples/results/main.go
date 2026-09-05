// Command results reproduces the synthetic compression examples in the README.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"log"
	"runtime"

	"imagecompressor/compressor"
)

func main() {
	flat := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	palette := []color.NRGBA{{R: 20, G: 40, B: 180, A: 255}, {G: 170, B: 235, A: 255}, {}}
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			flat.SetNRGBA(x, y, palette[(x/16+y/16)%len(palette)])
		}
	}
	gradient := image.NewNRGBA(image.Rect(0, 0, 128, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 128; x++ {
			gradient.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 2), G: uint8(y * 2), B: uint8((x + y) / 2), A: 255})
		}
	}
	noise := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	seed := uint32(1)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			seed = seed*1664525 + 1013904223
			noise.SetNRGBA(x, y, color.NRGBA{R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255})
		}
	}
	var flatPNG, gradientPNG, gradientJPEG, noisePNG, animation bytes.Buffer
	must(png.Encode(&flatPNG, flat))
	must(png.Encode(&gradientPNG, gradient))
	must(jpeg.Encode(&gradientJPEG, gradient, &jpeg.Options{Quality: 100}))
	must(png.Encode(&noisePNG, noise))
	frame1 := image.NewPaletted(image.Rect(0, 0, 16, 16), color.Palette{color.Black, color.White})
	frame2 := image.NewPaletted(frame1.Bounds(), frame1.Palette)
	frame2.SetColorIndex(8, 8, 1)
	must(gif.EncodeAll(&animation, &gif.GIF{Image: []*image.Paletted{frame1, frame2}, Delay: []int{10, 10}, LoopCount: 0}))
	fmt.Printf("Go: %s\n\n", runtime.Version())
	fmt.Println("| Synthetic input | Mode | Before (bytes) | After (bytes) | Saved |")
	fmt.Println("| --- | --- | ---: | ---: | ---: |")
	for _, tc := range []struct {
		name, mode string
		data       []byte
		opts       compressor.Options
	}{
		{"Three-color graphic, 128 × 128 PNG", "Lossless", flatPNG.Bytes(), compressor.Options{Lossless: true}},
		{"Gradient, 128 × 96 PNG", "Smart, 37 dB", gradientPNG.Bytes(), compressor.Options{Smart: true, MinPSNR: 37}},
		{"Gradient, 128 × 96 JPEG (quality 100)", "Smart, 37 dB", gradientJPEG.Bytes(), compressor.Options{Smart: true, MinPSNR: 37}},
		{"Seeded noise, 64 × 64 PNG", "Lossless", noisePNG.Bytes(), compressor.Options{Lossless: true}},
		{"Seeded noise, 64 × 64 PNG", "Lossy, 32 colors", noisePNG.Bytes(), compressor.Options{Colors: 32}},
		{"Two-frame animation, 16 × 16 GIF", "Pass-through", animation.Bytes(), compressor.Options{Smart: true}},
	} {
		var output bytes.Buffer
		must(compressor.Compress(&output, bytes.NewReader(tc.data), tc.opts))
		fmt.Printf("| %s | %s | %d | %d | %.1f%% |\n", tc.name, tc.mode, len(tc.data), output.Len(), 100*(1-float64(output.Len())/float64(len(tc.data))))
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
