// Package compressor optimizes PNG and JPEG images and preserves GIF files.
package compressor

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"sort"
)

// Options controls PNG and JPEG compression. The zero value selects manual
// lossy compression; set Smart or Lossless explicitly to use those modes.
type Options struct {
	// Smart tries several palette sizes and keeps the smallest result whose
	// pixel PSNR is at least MinPSNR. Avoid enabling Smart and Lossless together.
	Smart bool
	// MinPSNR is the minimum quality for Smart mode. Zero uses 37 dB, a
	// default. PSNR is a pixel metric, not a guarantee of visual equivalence.
	MinPSNR float64
	// Lossless preserves every decoded pixel exactly. If recompression is not
	// smaller, CompressPNG writes the original file unchanged.
	Lossless bool
	// Colors is the maximum palette size. Valid values are 2 through 256.
	// Zero uses 256. It is used only for manual lossy PNG compression.
	Colors int
	// Dither applies Floyd-Steinberg error diffusion. It can improve gradients,
	// but may produce a slightly larger PNG.
	Dither bool
	// JPEGQuality sets quality in manual lossy JPEG mode. Zero uses 80.
	JPEGQuality int
}

type sample struct {
	r, g, b, a uint8
	count      int
}

type bucket struct {
	colors []sample
	weight int
}

// CompressPNG optimizes a PNG. Lossless mode preserves every decoded pixel.
// The original bytes are returned when recompression would make the file larger.
func CompressPNG(dst io.Writer, src io.Reader, opts Options) error {
	original, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	img, err := png.Decode(bytes.NewReader(original))
	if err != nil {
		return err
	}
	return compressPNGBytes(dst, original, img, opts)
}

// Compress detects and compresses PNG and JPEG input. GIF files are accepted
// but copied unchanged to preserve animation. The output keeps the input format.
func Compress(dst io.Writer, src io.Reader, opts Options) error {
	original, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	img, format, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return err
	}
	switch format {
	case "png":
		return compressPNGBytes(dst, original, img, opts)
	case "jpeg":
		return compressJPEGBytes(dst, original, img, opts)
	case "gif":
		_, err = dst.Write(original)
		return err
	default:
		return fmt.Errorf("unsupported image format %q", format)
	}
}

func compressPNGBytes(dst io.Writer, original []byte, img image.Image, opts Options) error {

	if opts.Smart {
		return compressSmart(dst, original, img, opts)
	}

	var result image.Image = img
	if opts.Lossless {
		// An RGBA PNG with at most 256 exact colors can be represented as an
		// indexed PNG without changing a single pixel. This is often the biggest
		// lossless win for icons, logos, screenshots, and flat illustrations.
		if paletted, ok := exactPalette(img, 256); ok {
			result = paletted
		}
	} else {
		quantized, err := Quantize(img, opts)
		if err != nil {
			return err
		}
		result = quantized
	}

	encoded, err := encodePNG(result)
	if err != nil {
		return err
	}
	if len(encoded) >= len(original) {
		_, err = dst.Write(original)
		return err
	}
	_, err = dst.Write(encoded)
	return err
}

func compressJPEGBytes(dst io.Writer, original []byte, img image.Image, opts Options) error {
	if opts.Lossless {
		_, err := dst.Write(original)
		return err
	}
	if !opts.Smart {
		quality := opts.JPEGQuality
		if quality == 0 {
			quality = 80
		}
		if quality < 1 || quality > 100 {
			return errors.New("JPEG quality must be between 1 and 100")
		}
		encoded, err := encodeJPEG(img, quality)
		if err != nil {
			return err
		}
		if len(encoded) >= len(original) {
			encoded = original
		}
		_, err = dst.Write(encoded)
		return err
	}

	minPSNR := opts.MinPSNR
	if minPSNR == 0 {
		minPSNR = 37
	}
	if minPSNR < 20 || minPSNR > 60 {
		return errors.New("minimum PSNR must be between 20 and 60 dB")
	}
	best := original
	for _, quality := range []int{95, 92, 90, 88, 85, 82, 80, 78, 75, 72, 70, 68, 65, 60, 55, 50, 45, 40} {
		encoded, err := encodeJPEG(img, quality)
		if err != nil {
			return err
		}
		candidate, err := jpeg.Decode(bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		if pixelPSNR(img, candidate) >= minPSNR && len(encoded) < len(best) {
			best = encoded
		}
	}
	_, err := dst.Write(best)
	return err
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func compressSmart(dst io.Writer, original []byte, src image.Image, opts Options) error {
	minPSNR := opts.MinPSNR
	if minPSNR == 0 {
		minPSNR = 37
	}
	if minPSNR < 20 || minPSNR > 60 {
		return errors.New("minimum PSNR must be between 20 and 60 dB")
	}

	best := original
	// Testing every candidate is intentional: median-cut quality is not
	// perfectly monotonic as bucket boundaries move.
	for _, colors := range []int{256, 192, 160, 128, 96, 80, 64, 48, 32, 24, 16} {
		candidate, err := Quantize(src, Options{Colors: colors})
		if err != nil {
			return err
		}
		if pixelPSNR(src, candidate) < minPSNR {
			continue
		}
		encoded, err := encodePNG(candidate)
		if err != nil {
			return err
		}
		if len(encoded) < len(best) {
			best = encoded
		}
	}
	_, err := dst.Write(best)
	return err
}

func encodePNG(img image.Image) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&encoded, img); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func pixelPSNR(a, b image.Image) float64 {
	bounds := a.Bounds()
	if bounds.Dx() != b.Bounds().Dx() || bounds.Dy() != b.Bounds().Dy() {
		return 0
	}
	var squaredError float64
	var samples int64
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			ca := color.NRGBAModel.Convert(a.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			cb := color.NRGBAModel.Convert(b.At(b.Bounds().Min.X+x, b.Bounds().Min.Y+y)).(color.NRGBA)
			for _, d := range []int{
				int(ca.R) - int(cb.R), int(ca.G) - int(cb.G),
				int(ca.B) - int(cb.B), int(ca.A) - int(cb.A),
			} {
				squaredError += float64(d * d)
				samples++
			}
		}
	}
	if squaredError == 0 {
		return math.Inf(1)
	}
	mse := squaredError / float64(samples)
	return 10 * math.Log10(255*255/mse)
}

func exactPalette(src image.Image, limit int) (*image.Paletted, bool) {
	// Palette entries are 8-bit; keep 16-bit PNGs in their original color model.
	switch src.(type) {
	case *image.Gray16, *image.RGBA64, *image.NRGBA64:
		return nil, false
	}
	bounds := src.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), src, bounds.Min, draw.Src)

	indexes := make(map[uint32]uint8, limit)
	palette := make(color.Palette, 0, limit)
	for y := 0; y < nrgba.Rect.Dy(); y++ {
		for x := 0; x < nrgba.Rect.Dx(); x++ {
			i := nrgba.PixOffset(x, y)
			key := uint32(nrgba.Pix[i])<<24 | uint32(nrgba.Pix[i+1])<<16 |
				uint32(nrgba.Pix[i+2])<<8 | uint32(nrgba.Pix[i+3])
			if _, exists := indexes[key]; exists {
				continue
			}
			if len(palette) == limit {
				return nil, false
			}
			indexes[key] = uint8(len(palette))
			palette = append(palette, color.NRGBA{
				R: nrgba.Pix[i], G: nrgba.Pix[i+1], B: nrgba.Pix[i+2], A: nrgba.Pix[i+3],
			})
		}
	}

	out := image.NewPaletted(nrgba.Bounds(), palette)
	for y := 0; y < nrgba.Rect.Dy(); y++ {
		for x := 0; x < nrgba.Rect.Dx(); x++ {
			i := nrgba.PixOffset(x, y)
			key := uint32(nrgba.Pix[i])<<24 | uint32(nrgba.Pix[i+1])<<16 |
				uint32(nrgba.Pix[i+2])<<8 | uint32(nrgba.Pix[i+3])
			out.SetColorIndex(x, y, indexes[key])
		}
	}
	return out, true
}

// Quantize converts an image to an indexed palette using weighted median cut.
func Quantize(src image.Image, opts Options) (*image.Paletted, error) {
	if opts.Colors == 0 {
		opts.Colors = 256
	}
	if opts.Colors < 2 || opts.Colors > 256 {
		return nil, errors.New("colors must be between 2 and 256")
	}

	bounds := src.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), src, bounds.Min, draw.Src)
	palette := buildPalette(nrgba, opts.Colors)
	out := image.NewPaletted(nrgba.Bounds(), palette)
	if opts.Dither {
		dither(out, nrgba)
	} else {
		mapNearest(out, nrgba)
	}
	return out, nil
}

func buildPalette(img *image.NRGBA, limit int) color.Palette {
	hist := make(map[uint32]int)
	for y := 0; y < img.Rect.Dy(); y++ {
		for x := 0; x < img.Rect.Dx(); x++ {
			i := img.PixOffset(x, y)
			key := uint32(img.Pix[i])<<24 | uint32(img.Pix[i+1])<<16 |
				uint32(img.Pix[i+2])<<8 | uint32(img.Pix[i+3])
			hist[key]++
		}
	}

	colors := make([]sample, 0, len(hist))
	for key, count := range hist {
		colors = append(colors, sample{
			r: uint8(key >> 24), g: uint8(key >> 16),
			b: uint8(key >> 8), a: uint8(key), count: count,
		})
	}
	// Map iteration order is randomized in Go. Establish a stable secondary
	// order so equal channel values always split the same way.
	sort.Slice(colors, func(i, j int) bool {
		return rgbaKey(colors[i]) < rgbaKey(colors[j])
	})
	if len(colors) <= limit {
		return samplesToPalette(colors)
	}

	buckets := []bucket{{colors: colors, weight: totalWeight(colors)}}
	for len(buckets) < limit {
		idx := splittableBucket(buckets)
		if idx < 0 {
			break
		}
		left, right := splitBucket(buckets[idx])
		buckets[idx] = left
		buckets = append(buckets, right)
	}

	palette := make(color.Palette, 0, len(buckets))
	for _, b := range buckets {
		palette = append(palette, averageColor(b.colors))
	}
	return refinePalette(colors, palette, 8)
}

func refinePalette(samples []sample, palette color.Palette, iterations int) color.Palette {
	for iteration := 0; iteration < iterations; iteration++ {
		type sum struct{ r, g, b, a, weight uint64 }
		sums := make([]sum, len(palette))
		for _, s := range samples {
			idx := nearest(s.r, s.g, s.b, s.a, palette)
			w := uint64(s.count)
			sums[idx].r += uint64(s.r) * w
			sums[idx].g += uint64(s.g) * w
			sums[idx].b += uint64(s.b) * w
			sums[idx].a += uint64(s.a) * w
			sums[idx].weight += w
		}
		changed := false
		for i, s := range sums {
			if s.weight == 0 {
				continue
			}
			updated := color.NRGBA{
				R: uint8(s.r / s.weight), G: uint8(s.g / s.weight),
				B: uint8(s.b / s.weight), A: uint8(s.a / s.weight),
			}
			if color.NRGBAModel.Convert(palette[i]).(color.NRGBA) != updated {
				palette[i], changed = updated, true
			}
		}
		if !changed {
			break
		}
	}
	return palette
}

func rgbaKey(c sample) uint32 {
	return uint32(c.r)<<24 | uint32(c.g)<<16 | uint32(c.b)<<8 | uint32(c.a)
}

func samplesToPalette(colors []sample) color.Palette {
	p := make(color.Palette, 0, len(colors))
	for _, c := range colors {
		p = append(p, color.NRGBA{R: c.r, G: c.g, B: c.b, A: c.a})
	}
	return p
}

func totalWeight(colors []sample) int {
	total := 0
	for _, c := range colors {
		total += c.count
	}
	return total
}

func channelRange(colors []sample, channel int) int {
	min, max := 255, 0
	for _, c := range colors {
		v := []uint8{c.r, c.g, c.b, c.a}[channel]
		if int(v) < min {
			min = int(v)
		}
		if int(v) > max {
			max = int(v)
		}
	}
	return max - min
}

func splittableBucket(buckets []bucket) int {
	best, score := -1, -1
	for i, b := range buckets {
		if len(b.colors) < 2 {
			continue
		}
		rangeMax := 0
		for ch := 0; ch < 4; ch++ {
			if r := channelRange(b.colors, ch); r > rangeMax {
				rangeMax = r
			}
		}
		candidate := rangeMax * b.weight
		if candidate > score {
			best, score = i, candidate
		}
	}
	return best
}

func splitBucket(b bucket) (bucket, bucket) {
	channel, widest := 0, -1
	for ch := 0; ch < 4; ch++ {
		if r := channelRange(b.colors, ch); r > widest {
			channel, widest = ch, r
		}
	}
	sort.SliceStable(b.colors, func(i, j int) bool {
		a, z := b.colors[i], b.colors[j]
		return []uint8{a.r, a.g, a.b, a.a}[channel] < []uint8{z.r, z.g, z.b, z.a}[channel]
	})

	half, cumulative, cut := b.weight/2, 0, 1
	for i := 0; i < len(b.colors)-1; i++ {
		cumulative += b.colors[i].count
		if cumulative >= half {
			cut = i + 1
			break
		}
	}
	leftColors := append([]sample(nil), b.colors[:cut]...)
	rightColors := append([]sample(nil), b.colors[cut:]...)
	return bucket{leftColors, totalWeight(leftColors)}, bucket{rightColors, totalWeight(rightColors)}
}

func averageColor(colors []sample) color.Color {
	var r, g, b, a, total uint64
	for _, c := range colors {
		w := uint64(c.count)
		r += uint64(c.r) * w
		g += uint64(c.g) * w
		b += uint64(c.b) * w
		a += uint64(c.a) * w
		total += w
	}
	return color.NRGBA{uint8(r / total), uint8(g / total), uint8(b / total), uint8(a / total)}
}

func mapNearest(dst *image.Paletted, src *image.NRGBA) {
	for y := 0; y < src.Rect.Dy(); y++ {
		for x := 0; x < src.Rect.Dx(); x++ {
			i := src.PixOffset(x, y)
			dst.SetColorIndex(x, y, nearest(src.Pix[i], src.Pix[i+1], src.Pix[i+2], src.Pix[i+3], dst.Palette))
		}
	}
}

func nearest(r, g, b, a uint8, palette color.Palette) uint8 {
	best, bestDistance := 0, int64(1<<62)
	for i, entry := range palette {
		c := color.NRGBAModel.Convert(entry).(color.NRGBA)
		dr, dg, db, da := int64(r)-int64(c.R), int64(g)-int64(c.G), int64(b)-int64(c.B), int64(a)-int64(c.A)
		// Alpha gets extra weight because mismatching transparency is conspicuous.
		distance := dr*dr + dg*dg + db*db + 2*da*da
		if distance < bestDistance {
			best, bestDistance = i, distance
		}
	}
	return uint8(best)
}

func dither(dst *image.Paletted, src *image.NRGBA) {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	current := make([][4]float64, w+2)
	next := make([][4]float64, w+2)
	clamp := func(v float64) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v + 0.5)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := src.PixOffset(x, y)
			r := clamp(float64(src.Pix[i]) + current[x+1][0])
			g := clamp(float64(src.Pix[i+1]) + current[x+1][1])
			b := clamp(float64(src.Pix[i+2]) + current[x+1][2])
			a := src.Pix[i+3] // Do not diffuse alpha; it creates edge artifacts.
			pi := nearest(r, g, b, a, dst.Palette)
			dst.SetColorIndex(x, y, pi)
			pc := color.NRGBAModel.Convert(dst.Palette[pi]).(color.NRGBA)
			err := [4]float64{float64(r) - float64(pc.R), float64(g) - float64(pc.G), float64(b) - float64(pc.B), 0}
			for ch := 0; ch < 3; ch++ {
				current[x+2][ch] += err[ch] * 7 / 16
				next[x][ch] += err[ch] * 3 / 16
				next[x+1][ch] += err[ch] * 5 / 16
				next[x+2][ch] += err[ch] / 16
			}
		}
		current, next = next, current
		for i := range next {
			next[i] = [4]float64{}
		}
	}
}
